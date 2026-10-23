package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
)

// Repository is stateless; every method takes the DBTX to run on so callers control
// transaction boundaries.
type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

const userColumns = `id, phone_e164, status, username, kyc_status, last_login_at, created_at, updated_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.PhoneE164, &u.Status, &u.Username, &u.KYCStatus, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

// GetOrCreateByPhone returns the user for a verified phone number, creating it (and an
// empty profile) if needed. It is safe under concurrent calls for the same phone.
func (r *Repository) GetOrCreateByPhone(ctx context.Context, q postgres.DBTX, phoneE164 string) (User, bool, error) {
	u, err := scanUser(q.QueryRow(ctx, `
		INSERT INTO users (phone_e164, phone_verified_at, last_login_at)
		VALUES ($1, now(), now())
		ON CONFLICT (phone_e164) DO NOTHING
		RETURNING `+userColumns, phoneE164))
	switch {
	case err == nil:
		if _, err := q.Exec(ctx, `INSERT INTO profiles (user_id) VALUES ($1)`, u.ID); err != nil {
			return User{}, false, fmt.Errorf("create profile: %w", err)
		}
		return u, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		u, err = scanUser(q.QueryRow(ctx, `
			UPDATE users SET last_login_at = now(), phone_verified_at = COALESCE(phone_verified_at, now())
			WHERE phone_e164 = $1
			RETURNING `+userColumns, phoneE164))
		if err != nil {
			return User{}, false, fmt.Errorf("load user by phone: %w", err)
		}
		return u, false, nil
	default:
		return User{}, false, fmt.Errorf("create user: %w", err)
	}
}

func (r *Repository) GetByID(ctx context.Context, q postgres.DBTX, id uuid.UUID) (User, error) {
	u, err := scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, apperr.ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (r *Repository) GetProfile(ctx context.Context, q postgres.DBTX, userID uuid.UUID) (Profile, error) {
	var p Profile
	var updatedAt time.Time
	err := q.QueryRow(ctx, `
		SELECT first_name, last_name, email, date_of_birth, avatar_url, updated_at
		FROM profiles WHERE user_id = $1`, userID).
		Scan(&p.FirstName, &p.LastName, &p.Email, &p.DateOfBirth, &p.AvatarURL, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, nil
	}
	if err != nil {
		return Profile{}, fmt.Errorf("get profile: %w", err)
	}
	p.UpdatedAt = &updatedAt
	return p, nil
}

// SaveProfile upserts the editable profile fields. avatar_url is not client-editable yet.
func (r *Repository) SaveProfile(ctx context.Context, q postgres.DBTX, userID uuid.UUID, p Profile) error {
	_, err := q.Exec(ctx, `
		INSERT INTO profiles (user_id, first_name, last_name, email, date_of_birth)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE
		SET first_name = EXCLUDED.first_name,
		    last_name = EXCLUDED.last_name,
		    email = EXCLUDED.email,
		    date_of_birth = EXCLUDED.date_of_birth`,
		userID, p.FirstName, p.LastName, p.Email, p.DateOfBirth)
	if _, ok := postgres.ConstraintError(err, postgres.CodeCheckViolation); ok {
		return apperr.Validation("Profile is not valid", nil).Wrap(err)
	}
	if err != nil {
		return fmt.Errorf("save profile: %w", err)
	}
	return nil
}

// SetUsername assigns a username. The partial unique index on username_normalized is
// the final arbiter for concurrent claims.
func (r *Repository) SetUsername(ctx context.Context, q postgres.DBTX, userID uuid.UUID, display, normalized string) error {
	tag, err := q.Exec(ctx, `UPDATE users SET username = $2, username_normalized = $3 WHERE id = $1`, userID, display, normalized)
	if name, ok := postgres.ConstraintError(err, postgres.CodeUniqueViolation); ok && name == "users_username_normalized_key" {
		return apperr.ErrUsernameTaken
	}
	if _, ok := postgres.ConstraintError(err, postgres.CodeCheckViolation); ok {
		return apperr.ErrUsernameInvalid.Wrap(err)
	}
	if err != nil {
		return fmt.Errorf("set username: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.ErrNotFound
	}
	return nil
}

// UsernameExists checks the source of truth for a normalized username.
func (r *Repository) UsernameExists(ctx context.Context, q postgres.DBTX, normalized string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE username_normalized = $1)`, normalized).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check username: %w", err)
	}
	return exists, nil
}

// EachUsername streams every normalized username in keyset-paginated batches.
func (r *Repository) EachUsername(ctx context.Context, q postgres.DBTX, batch int, fn func(string) error) (int, error) {
	after, total := "", 0
	for {
		rows, err := q.Query(ctx, `
			SELECT username_normalized FROM users
			WHERE username_normalized IS NOT NULL AND username_normalized > $1
			ORDER BY username_normalized LIMIT $2`, after, batch)
		if err != nil {
			return total, fmt.Errorf("list usernames: %w", err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return total, fmt.Errorf("scan usernames: %w", err)
		}
		for _, n := range names {
			if err := fn(n); err != nil {
				return total, err
			}
		}
		total += len(names)
		if len(names) < batch {
			return total, nil
		}
		after = names[len(names)-1]
	}
}

// FindByUsername looks up a user by normalized username.
func (r *Repository) FindByUsername(ctx context.Context, q postgres.DBTX, normalized string) (User, error) {
	u, err := scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE username_normalized = $1`, normalized))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, apperr.ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user by username: %w", err)
	}
	return u, nil
}

// FindByPhone looks up a user by E.164 phone number.
func (r *Repository) FindByPhone(ctx context.Context, q postgres.DBTX, phoneE164 string) (User, error) {
	u, err := scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE phone_e164 = $1`, phoneE164))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, apperr.ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user by phone: %w", err)
	}
	return u, nil
}
