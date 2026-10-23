package bank

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
)

type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

const accountColumns = `id, user_id, holder_name, bank_name, sort_code, account_last4, is_primary, created_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.UserID, &a.HolderName, &a.BankName, &a.SortCode, &a.Last4, &a.IsPrimary, &a.CreatedAt)
	return a, err
}

// List returns active accounts, primary first.
func (r *Repository) List(ctx context.Context, q postgres.DBTX, userID uuid.UUID) ([]Account, error) {
	rows, err := q.Query(ctx, `
		SELECT `+accountColumns+` FROM bank_accounts
		WHERE user_id = $1 AND status = 'active'
		ORDER BY is_primary DESC, created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("list bank accounts: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Account, error) { return scanAccount(row) })
}

// Get returns an active account owned by userID.
func (r *Repository) Get(ctx context.Context, q postgres.DBTX, userID, id uuid.UUID) (Account, error) {
	a, err := scanAccount(q.QueryRow(ctx, `
		SELECT `+accountColumns+` FROM bank_accounts
		WHERE id = $1 AND user_id = $2 AND status = 'active'`, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, apperr.ErrBankAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get bank account: %w", err)
	}
	return a, nil
}

// lockUser serializes account changes for one user (count limit, primary flag).
func (r *Repository) lockUser(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		return fmt.Errorf("lock user: %w", err)
	}
	return nil
}

func (r *Repository) countActive(ctx context.Context, q postgres.DBTX, userID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM bank_accounts WHERE user_id = $1 AND status = 'active'`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count bank accounts: %w", err)
	}
	return n, nil
}

func (r *Repository) clearPrimary(ctx context.Context, q postgres.DBTX, userID uuid.UUID) error {
	if _, err := q.Exec(ctx, `UPDATE bank_accounts SET is_primary = false WHERE user_id = $1 AND is_primary`, userID); err != nil {
		return fmt.Errorf("clear primary: %w", err)
	}
	return nil
}

func (r *Repository) insert(ctx context.Context, q postgres.DBTX, a Account, hash []byte) (Account, error) {
	out, err := scanAccount(q.QueryRow(ctx, `
		INSERT INTO bank_accounts (user_id, holder_name, bank_name, sort_code, account_last4, account_number_hash, is_primary)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+accountColumns,
		a.UserID, a.HolderName, a.BankName, a.SortCode, a.Last4, hash, a.IsPrimary))
	if name, ok := postgres.ConstraintError(err, postgres.CodeUniqueViolation); ok && name == "bank_accounts_user_account_active_key" {
		return Account{}, apperr.ErrBankAccountExists
	}
	if err != nil {
		return Account{}, fmt.Errorf("insert bank account: %w", err)
	}
	return out, nil
}

func (r *Repository) setPrimary(ctx context.Context, q postgres.DBTX, id uuid.UUID) error {
	if _, err := q.Exec(ctx, `UPDATE bank_accounts SET is_primary = true WHERE id = $1`, id); err != nil {
		return fmt.Errorf("set primary: %w", err)
	}
	return nil
}

// remove soft-deletes an account; transactions keep referencing it for history.
func (r *Repository) remove(ctx context.Context, q postgres.DBTX, id uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE bank_accounts SET status = 'removed', removed_at = now(), is_primary = false WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("remove bank account: %w", err)
	}
	return nil
}

// promoteOldest makes the oldest remaining active account primary.
func (r *Repository) promoteOldest(ctx context.Context, q postgres.DBTX, userID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE bank_accounts SET is_primary = true
		WHERE id = (SELECT id FROM bank_accounts WHERE user_id = $1 AND status = 'active' ORDER BY created_at LIMIT 1)`, userID)
	if err != nil {
		return fmt.Errorf("promote primary: %w", err)
	}
	return nil
}
