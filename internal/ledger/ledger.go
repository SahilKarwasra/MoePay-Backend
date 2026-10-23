package ledger

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/internal/money"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
)

type Type string

const (
	TypeP2P            Type = "p2p_transfer"
	TypeAddMoney       Type = "add_money"
	TypeWithdraw       Type = "withdraw"
	TypeRequestPayment Type = "request_payment"
)

const clearingCode = "BANK_CLEARING"

// Posting describes one movement of money between two wallets.
type Posting struct {
	Type            Type
	AmountPence     int64
	FromWalletID    uuid.UUID
	ToWalletID      uuid.UUID
	InitiatorUserID uuid.UUID
	BankAccountID   *uuid.UUID
	MoneyRequestID  *uuid.UUID
	Note            *string
	IdempotencyKey  string
}

type Result struct {
	TransactionID uuid.UUID
	Reference     string
	FromBalance   int64
	ToBalance     int64
}

// Ledger is the only code that changes wallet balances. Every posting writes a
// transactions row, a debit and a credit entry, and updates the cached balances in
// the caller's database transaction; a deferred trigger rejects unbalanced postings.
type Ledger struct {
	cfg config.WalletConfig
	now func() time.Time

	mu         sync.Mutex
	clearingID uuid.UUID
}

func New(cfg config.WalletConfig) *Ledger {
	return &Ledger{cfg: cfg, now: time.Now}
}

func (l *Ledger) Limits() config.WalletConfig { return l.cfg }

// ClearingWalletID returns the system wallet that is the counterparty for bank flows.
func (l *Ledger) ClearingWalletID(ctx context.Context, q postgres.DBTX) (uuid.UUID, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clearingID != uuid.Nil {
		return l.clearingID, nil
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `SELECT id FROM wallets WHERE system_code = $1`, clearingCode).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("load clearing wallet: %w", err)
	}
	l.clearingID = id
	return id, nil
}

// ValidateAmount checks the per-transaction bounds.
func (l *Ledger) ValidateAmount(pence int64) error {
	if pence < l.cfg.MinPerTxnPence {
		return apperr.ErrAmountInvalid.WithDetails(map[string]any{"min": money.New(l.cfg.MinPerTxnPence)})
	}
	if pence > l.cfg.MaxPerTxnPence {
		return apperr.ErrLimitExceeded.WithDetails(map[string]any{
			"limit": "per_transaction", "max": money.New(l.cfg.MaxPerTxnPence),
		})
	}
	return nil
}

// StartOfDay returns the start of the calendar day containing t in the limits time zone.
func (l *Ledger) StartOfDay(t time.Time) time.Time {
	t = t.In(l.cfg.Location)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, l.cfg.Location)
}

// DailyOutgoing sums today's successful outgoing payments from a wallet.
func (l *Ledger) DailyOutgoing(ctx context.Context, q postgres.DBTX, walletID uuid.UUID) (int64, error) {
	var used int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_pence), 0) FROM transactions
		WHERE from_wallet_id = $1 AND status = 'succeeded' AND created_at >= $2
		  AND type IN ('p2p_transfer', 'withdraw', 'request_payment')`,
		walletID, l.StartOfDay(l.now())).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("daily outgoing: %w", err)
	}
	return used, nil
}

type lockedWallet struct {
	id      uuid.UUID
	kind    string
	status  string
	balance int64
}

// Post moves money. It locks both wallets in id order (so concurrent opposite
// transfers cannot deadlock), enforces balances and limits, and writes the ledger.
func (l *Ledger) Post(ctx context.Context, tx pgx.Tx, p Posting) (Result, error) {
	if err := l.ValidateAmount(p.AmountPence); err != nil {
		return Result{}, err
	}
	if p.FromWalletID == p.ToWalletID {
		return Result{}, apperr.ErrCannotPaySelf
	}

	rows, err := tx.Query(ctx, `
		SELECT id, kind, status, balance_pence FROM wallets
		WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, p.FromWalletID, p.ToWalletID)
	if err != nil {
		return Result{}, fmt.Errorf("lock wallets: %w", err)
	}
	locked := map[uuid.UUID]lockedWallet{}
	for rows.Next() {
		var w lockedWallet
		if err := rows.Scan(&w.id, &w.kind, &w.status, &w.balance); err != nil {
			rows.Close()
			return Result{}, fmt.Errorf("scan wallet: %w", err)
		}
		locked[w.id] = w
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("lock wallets: %w", err)
	}
	from, okFrom := locked[p.FromWalletID]
	to, okTo := locked[p.ToWalletID]
	if !okFrom || !okTo {
		return Result{}, apperr.ErrWalletNotReady
	}
	if from.status != "active" || to.status != "active" {
		return Result{}, apperr.ErrWalletFrozen
	}

	if from.kind == "user" {
		if from.balance < p.AmountPence {
			return Result{}, apperr.ErrInsufficientBalance.WithDetails(map[string]any{"balance": money.New(from.balance)})
		}
		used, err := l.DailyOutgoing(ctx, tx, from.id)
		if err != nil {
			return Result{}, err
		}
		if used+p.AmountPence > l.cfg.DailyOutgoingPence {
			return Result{}, apperr.ErrLimitExceeded.WithDetails(map[string]any{
				"limit":      "daily_outgoing",
				"max":        money.New(l.cfg.DailyOutgoingPence),
				"used_today": money.New(used),
				"remaining":  money.New(max(l.cfg.DailyOutgoingPence-used, 0)),
			})
		}
	}
	if to.kind == "user" && to.balance+p.AmountPence > l.cfg.MaxBalancePence {
		if p.Type == TypeAddMoney {
			return Result{}, apperr.ErrWalletBalanceLimit.WithDetails(map[string]any{
				"max_balance": money.New(l.cfg.MaxBalancePence),
				"can_add":     money.New(max(l.cfg.MaxBalancePence-to.balance, 0)),
			})
		}
		return Result{}, apperr.ErrRecipientCannotTake
	}

	txnID, ref, err := l.insertTransaction(ctx, tx, p, "succeeded", nil, nil)
	if err != nil {
		return Result{}, err
	}
	res := Result{TransactionID: txnID, Reference: ref, FromBalance: from.balance - p.AmountPence, ToBalance: to.balance + p.AmountPence}
	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries (transaction_id, wallet_id, direction, amount_pence, balance_after_pence)
		VALUES ($1, $2, 'debit', $4, $5), ($1, $3, 'credit', $4, $6)`,
		txnID, from.id, to.id, p.AmountPence, res.FromBalance, res.ToBalance)
	if err != nil {
		return Result{}, fmt.Errorf("insert ledger entries: %w", err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE wallets SET balance_pence = CASE id WHEN $1 THEN $3::bigint ELSE $4::bigint END
		WHERE id IN ($1, $2)`, from.id, to.id, res.FromBalance, res.ToBalance)
	if err != nil {
		if _, ok := postgres.ConstraintError(err, postgres.CodeCheckViolation); ok {
			return Result{}, apperr.ErrInsufficientBalance.Wrap(err)
		}
		return Result{}, fmt.Errorf("update balances: %w", err)
	}
	return res, nil
}

// RecordFailed stores a failed attempt (e.g. declined by the bank) without moving money,
// so it still appears in the user's history.
func (l *Ledger) RecordFailed(ctx context.Context, tx pgx.Tx, p Posting, code, reason string) (Result, error) {
	id, ref, err := l.insertTransaction(ctx, tx, p, "failed", &code, &reason)
	return Result{TransactionID: id, Reference: ref}, err
}

func (l *Ledger) insertTransaction(ctx context.Context, tx pgx.Tx, p Posting, status string, failureCode, failureReason *string) (uuid.UUID, string, error) {
	var key *string
	if p.IdempotencyKey != "" {
		key = &p.IdempotencyKey
	}
	for attempt := 0; ; attempt++ {
		ref := newReference(l.now())
		var id uuid.UUID
		// A savepoint keeps the outer transaction usable if the reference collides.
		err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			return sp.QueryRow(ctx, `
				INSERT INTO transactions (reference, type, status, amount_pence, from_wallet_id, to_wallet_id,
					initiator_user_id, bank_account_id, money_request_id, note, failure_code, failure_reason,
					idempotency_key, completed_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now())
				RETURNING id`,
				ref, string(p.Type), status, p.AmountPence, p.FromWalletID, p.ToWalletID,
				p.InitiatorUserID, p.BankAccountID, p.MoneyRequestID, p.Note, failureCode, failureReason, key).Scan(&id)
		})
		name, unique := postgres.ConstraintError(err, postgres.CodeUniqueViolation)
		switch {
		case err == nil:
			return id, ref, nil
		case unique && name == "transactions_reference_key" && attempt < 3:
			continue
		case unique && name == "transactions_idempotency_key":
			return uuid.Nil, "", apperr.ErrIdempotencyInFlight
		case unique && name == "transactions_money_request_succeeded_key":
			return uuid.Nil, "", apperr.ErrRequestNotPending
		default:
			return uuid.Nil, "", fmt.Errorf("insert transaction: %w", err)
		}
	}
}

const refAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32

// newReference returns a human-friendly id such as MP261023K7Q2XH9A.
func newReference(now time.Time) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	out := make([]byte, 8)
	for i, v := range b {
		out[i] = refAlphabet[int(v)%len(refAlphabet)]
	}
	return "MP" + now.UTC().Format("060102") + string(out)
}
