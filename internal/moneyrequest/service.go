package moneyrequest

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/internal/history"
	"github.com/sahilkarwasra/moepay/internal/idempotency"
	"github.com/sahilkarwasra/moepay/internal/ledger"
	"github.com/sahilkarwasra/moepay/internal/money"
	"github.com/sahilkarwasra/moepay/internal/payment"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
	"github.com/sahilkarwasra/moepay/internal/wallet"
)

// maxPendingPerPair stops one user flooding another with requests.
const maxPendingPerPair = 5

type Service struct {
	pool     *pgxpool.Pool
	repo     *Repository
	ledger   *ledger.Ledger
	wallets  *wallet.Service
	payments *payment.Service
	history  *history.Reader
	events   payment.Publisher
	cfg      config.WalletConfig
	created  []Hook
	resolved []Hook
}

// Event identifies a money request whose state changed in the current transaction.
type Event struct {
	RequestID   uuid.UUID
	RequesterID uuid.UUID
	PayerID     uuid.UUID
	Status      string
}

// Hook runs inside the transaction that changed the request. The returned func, if
// any, runs after the transaction commits.
type Hook func(ctx context.Context, tx pgx.Tx, ev Event) (func(), error)

// OnCreated and OnResolved register hooks for new requests and for requests that
// were paid, declined, cancelled or expired.
func (s *Service) OnCreated(h Hook)  { s.created = append(s.created, h) }
func (s *Service) OnResolved(h Hook) { s.resolved = append(s.resolved, h) }

// afterCommit collects callbacks to run once the surrounding transaction commits.
type afterCommit []func()

func (a *afterCommit) run(ctx context.Context, tx pgx.Tx, hooks []Hook, ev Event) error {
	for _, h := range hooks {
		f, err := h(ctx, tx, ev)
		if err != nil {
			return err
		}
		if f != nil {
			*a = append(*a, f)
		}
	}
	return nil
}

func (a afterCommit) fire() {
	for _, f := range a {
		f()
	}
}

func NewService(pool *pgxpool.Pool, repo *Repository, l *ledger.Ledger, wallets *wallet.Service,
	payments *payment.Service, h *history.Reader, events payment.Publisher, cfg config.WalletConfig) *Service {
	return &Service{pool: pool, repo: repo, ledger: l, wallets: wallets, payments: payments, history: h, events: events, cfg: cfg}
}

type CreateInput struct {
	From        payment.RecipientRef `json:"from"`
	AmountPence int64                `json:"amount_pence"`
	Note        string               `json:"note"`
}

// Create asks another user to pay the caller.
func (s *Service) Create(ctx context.Context, me uuid.UUID, in CreateInput) (View, error) {
	if err := s.ledger.ValidateAmount(in.AmountPence); err != nil {
		return View{}, err
	}
	note, err := payment.CleanNote(in.Note)
	if err != nil {
		return View{}, err
	}
	if _, err := s.wallets.ForUser(ctx, s.pool, me); err != nil {
		return View{}, err
	}
	payer, err := s.payments.Resolve(ctx, s.pool, me, in.From)
	if err != nil {
		return View{}, err
	}
	payerID := uuid.MustParse(payer.UserID)

	var (
		view  View
		after afterCommit
	)
	err = postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		n, err := s.repo.CountPendingBetween(ctx, tx, me, payerID)
		if err != nil {
			return err
		}
		if n >= maxPendingPerPair {
			return apperr.ErrRateLimited.WithDetails(map[string]any{"reason": "too many pending requests to this person"})
		}
		id, err := s.repo.Insert(ctx, tx, me, payerID, in.AmountPence, note, s.cfg.MoneyRequestTTL)
		if err != nil {
			return err
		}
		if err := after.run(ctx, tx, s.created, Event{RequestID: id, RequesterID: me, PayerID: payerID, Status: StatusPending}); err != nil {
			return err
		}
		view, err = s.repo.Get(ctx, tx, me, id, false)
		return err
	})
	if err != nil {
		return View{}, err
	}
	s.publishFor(ctx, payerID, view.ID, "money_request.received")
	after.fire()
	return view, nil
}

func (s *Service) Get(ctx context.Context, me, id uuid.UUID) (View, error) {
	return s.repo.Get(ctx, s.pool, me, id, false)
}

func (s *Service) List(ctx context.Context, me uuid.UUID, f ListFilter) (Page, error) {
	if f.Direction != "incoming" && f.Direction != "outgoing" {
		return Page{}, apperr.Validation("direction is required", map[string]string{"direction": "incoming or outgoing"})
	}
	switch f.Status {
	case "", StatusPending, StatusPaid, StatusDeclined, StatusCancelled, StatusExpired:
	default:
		return Page{}, apperr.Validation("Invalid status", map[string]string{"status": "pending, paid, declined, cancelled or expired"})
	}
	if f.Limit <= 0 {
		f.Limit = history.DefaultLimit
	}
	f.Limit = min(f.Limit, history.MaxLimit)
	return s.repo.List(ctx, s.pool, me, f)
}

func (s *Service) Counts(ctx context.Context, q postgres.DBTX, me uuid.UUID) (Counts, error) {
	return s.repo.Counts(ctx, q, me)
}

type PayInput struct {
	PIN string `json:"pin"`
}

// PayResult is returned when a request is paid.
type PayResult struct {
	payment.Result
	Request View `json:"request"`
}

// Pay settles an incoming request from the caller's wallet.
func (s *Service) Pay(ctx context.Context, me, id uuid.UUID, key string, in PayInput) (PayResult, bool, error) {
	if err := idempotency.ValidateKey(key); err != nil {
		return PayResult{}, false, err
	}
	req, err := s.repo.Get(ctx, s.pool, me, id, false)
	if err != nil {
		return PayResult{}, false, err
	}
	if req.Direction != "incoming" {
		return PayResult{}, false, apperr.ErrMoneyRequestNotFound
	}
	payerWallet, err := s.wallets.ForUser(ctx, s.pool, me)
	if err != nil {
		return PayResult{}, false, err
	}
	if err := s.wallets.VerifyPIN(ctx, me, in.PIN); err != nil {
		return PayResult{}, false, err
	}

	var (
		requesterWallet wallet.Wallet
		after           afterCommit
	)
	res, replayed, err := idempotency.Run(ctx, s.pool, me, key, "POST /money-requests/pay", map[string]string{"request_id": id.String()},
		func(tx pgx.Tx) (PayResult, error) {
			req, err := s.repo.Get(ctx, tx, me, id, true)
			if err != nil {
				return PayResult{}, err
			}
			if req.Status != StatusPending {
				return PayResult{}, apperr.ErrRequestNotPending.WithDetails(map[string]any{"status": req.Status})
			}
			if requesterWallet, err = s.wallets.ForUser(ctx, tx, req.requesterID); err != nil {
				return PayResult{}, err
			}
			posted, err := s.ledger.Post(ctx, tx, ledger.Posting{
				Type: ledger.TypeRequestPayment, AmountPence: req.amount, FromWalletID: payerWallet.ID,
				ToWalletID: requesterWallet.ID, InitiatorUserID: me, MoneyRequestID: &id, Note: req.Note, IdempotencyKey: key,
			})
			if err != nil {
				return PayResult{}, err
			}
			if err := s.repo.Resolve(ctx, tx, id, StatusPaid, &posted.TransactionID); err != nil {
				return PayResult{}, err
			}
			if err := after.run(ctx, tx, s.resolved, Event{RequestID: id, RequesterID: req.requesterID, PayerID: me, Status: StatusPaid}); err != nil {
				return PayResult{}, err
			}
			view, err := s.history.Get(ctx, tx, payerWallet.ID, posted.TransactionID)
			if err != nil {
				return PayResult{}, err
			}
			updated, err := s.repo.Get(ctx, tx, me, id, false)
			if err != nil {
				return PayResult{}, err
			}
			return PayResult{Result: payment.Result{Transaction: view, Balance: money.New(posted.FromBalance)}, Request: updated}, nil
		})
	if err != nil {
		return PayResult{}, replayed, err
	}
	if !replayed {
		s.events.PublishToUser(ctx, me, "wallet.debited", res.Result)
		s.payments.NotifyCounterparty(ctx, req.requesterID, requesterWallet.ID, res.Transaction.ID, "wallet.credited")
		s.publishFor(ctx, req.requesterID, req.ID, "money_request.updated")
		after.fire()
	}
	return res, replayed, nil
}

// Decline is used by the payer; Cancel by the requester.
func (s *Service) Decline(ctx context.Context, me, id uuid.UUID) (View, error) {
	return s.resolve(ctx, me, id, "incoming", StatusDeclined)
}

func (s *Service) Cancel(ctx context.Context, me, id uuid.UUID) (View, error) {
	return s.resolve(ctx, me, id, "outgoing", StatusCancelled)
}

func (s *Service) resolve(ctx context.Context, me, id uuid.UUID, direction, status string) (View, error) {
	var (
		view  View
		after afterCommit
	)
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		req, err := s.repo.Get(ctx, tx, me, id, true)
		if err != nil {
			return err
		}
		if req.Direction != direction {
			return apperr.ErrForbidden
		}
		if req.Status != StatusPending {
			return apperr.ErrRequestNotPending.WithDetails(map[string]any{"status": req.Status})
		}
		if err := s.repo.Resolve(ctx, tx, id, status, nil); err != nil {
			return err
		}
		if err := after.run(ctx, tx, s.resolved, Event{RequestID: id, RequesterID: req.requesterID, PayerID: req.payerID, Status: status}); err != nil {
			return err
		}
		view, err = s.repo.Get(ctx, tx, me, id, false)
		return err
	})
	if err != nil {
		return View{}, err
	}
	other := view.requesterID
	if direction == "outgoing" {
		other = view.payerID
	}
	s.publishFor(ctx, other, view.ID, "money_request.updated")
	after.fire()
	return view, nil
}

// publishFor sends the request to userID as they see it.
func (s *Service) publishFor(ctx context.Context, userID uuid.UUID, requestID, event string) {
	id, err := uuid.Parse(requestID)
	if err != nil {
		return
	}
	v, err := s.repo.Get(ctx, s.pool, userID, id, false)
	if err != nil {
		return
	}
	s.events.PublishToUser(ctx, userID, event, v)
}

// ExpireDue is run periodically by a background job.
func (s *Service) ExpireDue(ctx context.Context) (int64, error) {
	var (
		n     int64
		after afterCommit
	)
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		expired, err := s.repo.ExpireDue(ctx, tx)
		if err != nil {
			return err
		}
		for _, ev := range expired {
			if err := after.run(ctx, tx, s.resolved, ev); err != nil {
				return err
			}
		}
		n = int64(len(expired))
		return nil
	})
	if err != nil {
		return 0, err
	}
	after.fire()
	return n, nil
}
