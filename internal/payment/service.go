package payment

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/bank"
	"github.com/sahilkarwasra/moepay/internal/history"
	"github.com/sahilkarwasra/moepay/internal/idempotency"
	"github.com/sahilkarwasra/moepay/internal/ledger"
	"github.com/sahilkarwasra/moepay/internal/money"
	"github.com/sahilkarwasra/moepay/internal/user"
	"github.com/sahilkarwasra/moepay/internal/wallet"
)

// Publisher pushes realtime events to a user's connected devices.
type Publisher interface {
	PublishToUser(ctx context.Context, userID uuid.UUID, eventType string, data any)
}

type Service struct {
	pool    *pgxpool.Pool
	ledger  *ledger.Ledger
	wallets *wallet.Service
	banks   *bank.Repository
	gateway bank.Gateway
	users   *user.Repository
	history *history.Reader
	events  Publisher
	region  string
	logger  *slog.Logger
}

type Deps struct {
	Pool          *pgxpool.Pool
	Ledger        *ledger.Ledger
	Wallets       *wallet.Service
	Banks         *bank.Repository
	Gateway       bank.Gateway
	Users         *user.Repository
	History       *history.Reader
	Events        Publisher
	DefaultRegion string
	Logger        *slog.Logger
}

func NewService(d Deps) *Service {
	return &Service{
		pool: d.Pool, ledger: d.Ledger, wallets: d.Wallets, banks: d.Banks, gateway: d.Gateway,
		users: d.Users, history: d.History, events: d.Events, region: d.DefaultRegion, logger: d.Logger,
	}
}

// Result is returned by every money-moving endpoint.
type Result struct {
	Transaction history.View `json:"transaction"`
	Balance     money.Amount `json:"balance"`
}

type TransferInput struct {
	To          RecipientRef `json:"to"`
	AmountPence int64        `json:"amount_pence"`
	Note        string       `json:"note"`
	PIN         string       `json:"pin"`
}

// fingerprint is what identifies a request for idempotency; the PIN is excluded so
// a retry after a mistyped PIN is not treated as a different request.
type transferFingerprint struct {
	To          RecipientRef `json:"to"`
	AmountPence int64        `json:"amount_pence"`
	Note        string       `json:"note"`
}

// Transfer sends money from the caller's wallet to another MoePay user.
func (s *Service) Transfer(ctx context.Context, me uuid.UUID, key string, in TransferInput) (Result, bool, error) {
	if err := idempotency.ValidateKey(key); err != nil {
		return Result{}, false, err
	}
	note, err := CleanNote(in.Note)
	if err != nil {
		return Result{}, false, err
	}
	if err := s.ledger.ValidateAmount(in.AmountPence); err != nil {
		return Result{}, false, err
	}
	from, err := s.wallets.ForUser(ctx, s.pool, me)
	if err != nil {
		return Result{}, false, err
	}
	to, err := s.Resolve(ctx, s.pool, me, in.To)
	if err != nil {
		return Result{}, false, err
	}
	if err := s.wallets.VerifyPIN(ctx, me, in.PIN); err != nil {
		return Result{}, false, err
	}

	fp := transferFingerprint{To: in.To, AmountPence: in.AmountPence, Note: in.Note}
	res, replayed, err := idempotency.Run(ctx, s.pool, me, key, "POST /transfers", fp, func(tx pgx.Tx) (Result, error) {
		posted, err := s.ledger.Post(ctx, tx, ledger.Posting{
			Type: ledger.TypeP2P, AmountPence: in.AmountPence, FromWalletID: from.ID, ToWalletID: to.walletID,
			InitiatorUserID: me, Note: note, IdempotencyKey: key,
		})
		if err != nil {
			return Result{}, err
		}
		return s.result(ctx, tx, from.ID, posted.TransactionID, posted.FromBalance)
	})
	if err != nil {
		return Result{}, replayed, err
	}
	if !replayed {
		s.events.PublishToUser(ctx, me, "wallet.debited", res)
		s.NotifyCounterparty(ctx, uuid.MustParse(to.UserID), to.walletID, res.Transaction.ID, "wallet.credited")
	}
	return res, replayed, nil
}

type BankTransferInput struct {
	BankAccountID string `json:"bank_account_id"`
	AmountPence   int64  `json:"amount_pence"`
	PIN           string `json:"pin"`
}

type bankFingerprint struct {
	BankAccountID string `json:"bank_account_id"`
	AmountPence   int64  `json:"amount_pence"`
}

// AddMoney pulls money from a linked bank account into the wallet.
func (s *Service) AddMoney(ctx context.Context, me uuid.UUID, key string, in BankTransferInput) (Result, bool, error) {
	return s.bankTransfer(ctx, me, key, in, ledger.TypeAddMoney, "POST /wallet/add-money")
}

// Withdraw pushes money from the wallet to a linked bank account.
func (s *Service) Withdraw(ctx context.Context, me uuid.UUID, key string, in BankTransferInput) (Result, bool, error) {
	return s.bankTransfer(ctx, me, key, in, ledger.TypeWithdraw, "POST /wallet/withdraw")
}

func (s *Service) bankTransfer(ctx context.Context, me uuid.UUID, key string, in BankTransferInput, typ ledger.Type, endpoint string) (Result, bool, error) {
	if err := idempotency.ValidateKey(key); err != nil {
		return Result{}, false, err
	}
	if err := s.ledger.ValidateAmount(in.AmountPence); err != nil {
		return Result{}, false, err
	}
	w, err := s.wallets.ForUser(ctx, s.pool, me)
	if err != nil {
		return Result{}, false, err
	}
	acctID, err := uuid.Parse(strings.TrimSpace(in.BankAccountID))
	if err != nil {
		return Result{}, false, apperr.ErrBankAccountNotFound
	}
	acct, err := s.banks.Get(ctx, s.pool, me, acctID)
	if err != nil {
		return Result{}, false, err
	}
	if err := s.wallets.VerifyPIN(ctx, me, in.PIN); err != nil {
		return Result{}, false, err
	}

	fp := bankFingerprint{BankAccountID: acctID.String(), AmountPence: in.AmountPence}
	res, replayed, err := idempotency.Run(ctx, s.pool, me, key, endpoint, fp, func(tx pgx.Tx) (Result, error) {
		clearing, err := s.ledger.ClearingWalletID(ctx, tx)
		if err != nil {
			return Result{}, err
		}
		p := ledger.Posting{
			Type: typ, AmountPence: in.AmountPence, InitiatorUserID: me, BankAccountID: &acct.ID, IdempotencyKey: key,
			FromWalletID: clearing, ToWalletID: w.ID,
		}
		if typ == ledger.TypeWithdraw {
			p.FromWalletID, p.ToWalletID = w.ID, clearing
		}

		// Post in a savepoint first so limits and balances are enforced before the
		// bank is called; if the bank declines, undo the posting and record a failure.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return Result{}, err
		}
		posted, err := s.ledger.Post(ctx, sp, p)
		if err != nil {
			_ = sp.Rollback(ctx)
			return Result{}, err
		}
		if typ == ledger.TypeAddMoney {
			err = s.gateway.PullFunds(ctx, acct, in.AmountPence, posted.Reference)
		} else {
			err = s.gateway.PushFunds(ctx, acct, in.AmountPence, posted.Reference)
		}
		var decline *bank.DeclineError
		if errors.As(err, &decline) {
			if rbErr := sp.Rollback(ctx); rbErr != nil {
				return Result{}, rbErr
			}
			failed, err := s.ledger.RecordFailed(ctx, tx, p, decline.Code, decline.Reason)
			if err != nil {
				return Result{}, err
			}
			view, err := s.history.Get(ctx, tx, w.ID, failed.TransactionID)
			if err != nil {
				return Result{}, err
			}
			return Result{}, idempotency.Persist(apperr.ErrBankDeclined.WithDetails(map[string]any{
				"reason": decline.Reason, "bank_code": decline.Code, "transaction": view,
			}))
		}
		if err != nil {
			_ = sp.Rollback(ctx)
			return Result{}, apperr.ErrUnavailable.Wrap(err)
		}
		if err := sp.Commit(ctx); err != nil {
			return Result{}, err
		}
		balance := posted.ToBalance
		if typ == ledger.TypeWithdraw {
			balance = posted.FromBalance
		}
		return s.result(ctx, tx, w.ID, posted.TransactionID, balance)
	})
	if err != nil {
		return Result{}, replayed, err
	}
	if !replayed {
		event := "wallet.credited"
		if typ == ledger.TypeWithdraw {
			event = "wallet.debited"
		}
		s.events.PublishToUser(ctx, me, event, res)
	}
	return res, replayed, nil
}

func (s *Service) result(ctx context.Context, tx pgx.Tx, walletID, txnID uuid.UUID, balance int64) (Result, error) {
	view, err := s.history.Get(ctx, tx, walletID, txnID)
	if err != nil {
		return Result{}, err
	}
	return Result{Transaction: view, Balance: money.New(balance)}, nil
}

// NotifyCounterparty sends the other party their own view of a committed transaction.
func (s *Service) NotifyCounterparty(ctx context.Context, userID, walletID uuid.UUID, txnID, event string) {
	id, err := uuid.Parse(txnID)
	if err != nil {
		return
	}
	view, err := s.history.Get(ctx, s.pool, walletID, id)
	if err != nil {
		s.logger.WarnContext(ctx, "load counterparty view for event", "err", err)
		return
	}
	w, err := s.wallets.ForUser(ctx, s.pool, userID)
	if err != nil {
		return
	}
	s.events.PublishToUser(ctx, userID, event, Result{Transaction: view, Balance: money.New(w.BalancePence)})
}

// CleanNote trims a payment note and enforces its length.
func CleanNote(raw string) (*string, error) {
	n := strings.Join(strings.Fields(raw), " ")
	if n == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(n) > 140 {
		return nil, apperr.Validation("Note is too long", map[string]string{"note": "at most 140 characters"})
	}
	return &n, nil
}
