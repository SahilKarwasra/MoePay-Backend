package wallet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
)

type Wallet struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	Status            string
	BalancePence      int64
	PINSet            bool
	PINFailedAttempts int
	PINLockedUntil    *time.Time
	CreatedAt         time.Time
}

var errNotFound = errors.New("wallet not found")

type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

const walletColumns = `id, user_id, status, balance_pence, pin_hash IS NOT NULL, pin_failed_attempts, pin_locked_until, created_at`

func scanWallet(row pgx.Row) (Wallet, error) {
	var w Wallet
	err := row.Scan(&w.ID, &w.UserID, &w.Status, &w.BalancePence, &w.PINSet, &w.PINFailedAttempts, &w.PINLockedUntil, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, errNotFound
	}
	return w, err
}

// Create opens a wallet for userID if none exists.
func (r *Repository) Create(ctx context.Context, q postgres.DBTX, userID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		INSERT INTO wallets (user_id, kind) VALUES ($1, 'user')
		ON CONFLICT (user_id) WHERE user_id IS NOT NULL DO NOTHING`, userID)
	if err != nil {
		return fmt.Errorf("create wallet: %w", err)
	}
	return nil
}

func (r *Repository) GetByUser(ctx context.Context, q postgres.DBTX, userID uuid.UUID) (Wallet, error) {
	w, err := scanWallet(q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE user_id = $1`, userID))
	if err != nil && !errors.Is(err, errNotFound) {
		return Wallet{}, fmt.Errorf("get wallet: %w", err)
	}
	return w, err
}

func (r *Repository) onboardingCompleted(ctx context.Context, q postgres.DBTX, userID uuid.UUID) (bool, error) {
	var done bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM onboarding_progress WHERE user_id = $1 AND status = 'completed')`, userID).Scan(&done)
	if err != nil {
		return false, fmt.Errorf("check onboarding: %w", err)
	}
	return done, nil
}

type pinState struct {
	walletID    uuid.UUID
	hash        *string
	attempts    int
	lockedUntil *time.Time
}

// lockPIN locks the wallet row so PIN attempts are counted exactly.
func (r *Repository) lockPIN(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (pinState, error) {
	var s pinState
	err := tx.QueryRow(ctx, `
		SELECT id, pin_hash, pin_failed_attempts, pin_locked_until FROM wallets
		WHERE user_id = $1 FOR UPDATE`, userID).Scan(&s.walletID, &s.hash, &s.attempts, &s.lockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return pinState{}, errNotFound
	}
	if err != nil {
		return pinState{}, fmt.Errorf("lock wallet pin: %w", err)
	}
	return s, nil
}

func (r *Repository) setPIN(ctx context.Context, q postgres.DBTX, walletID uuid.UUID, hash string) error {
	_, err := q.Exec(ctx, `
		UPDATE wallets SET pin_hash = $2, pin_set_at = now(), pin_failed_attempts = 0, pin_locked_until = NULL
		WHERE id = $1`, walletID, hash)
	if err != nil {
		return fmt.Errorf("set pin: %w", err)
	}
	return nil
}

func (r *Repository) recordPINFailure(ctx context.Context, q postgres.DBTX, walletID uuid.UUID, attempts int, lockedUntil *time.Time) error {
	_, err := q.Exec(ctx, `UPDATE wallets SET pin_failed_attempts = $2, pin_locked_until = $3 WHERE id = $1`,
		walletID, attempts, lockedUntil)
	if err != nil {
		return fmt.Errorf("record pin failure: %w", err)
	}
	return nil
}

func (r *Repository) clearPINFailures(ctx context.Context, q postgres.DBTX, walletID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE wallets SET pin_failed_attempts = 0, pin_locked_until = NULL
		WHERE id = $1 AND (pin_failed_attempts <> 0 OR pin_locked_until IS NOT NULL)`, walletID)
	if err != nil {
		return fmt.Errorf("clear pin failures: %w", err)
	}
	return nil
}
