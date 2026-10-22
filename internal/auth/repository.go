package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
)

type Challenge struct {
	ID          uuid.UUID
	PhoneE164   string
	CodeHash    []byte
	Attempts    int
	MaxAttempts int
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
	CreatedAt   time.Time
}

type Session struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	ExpiresAt     time.Time
	RevokedAt     *time.Time
	RevokedReason *string
}

type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

func (r *Repository) CreateChallenge(ctx context.Context, q postgres.DBTX, phone string, hash []byte, maxAttempts int, ttl time.Duration, ip *netip.Addr) error {
	_, err := q.Exec(ctx, `
		INSERT INTO otp_challenges (phone_e164, code_hash, max_attempts, expires_at, request_ip)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5)`,
		phone, hash, maxAttempts, ttl.Seconds(), ip)
	if err != nil {
		return fmt.Errorf("create otp challenge: %w", err)
	}
	return nil
}

// RecentChallengeStats returns when the latest challenge for phone was created and how
// many were created in the last hour. Used as a Redis-independent abuse guard.
func (r *Repository) RecentChallengeStats(ctx context.Context, q postgres.DBTX, phone string) (last *time.Time, lastHour int, err error) {
	err = q.QueryRow(ctx, `
		SELECT max(created_at), count(*) FILTER (WHERE created_at > now() - interval '1 hour')
		FROM otp_challenges WHERE phone_e164 = $1 AND created_at > now() - interval '1 hour'`, phone).
		Scan(&last, &lastHour)
	if err != nil {
		return nil, 0, fmt.Errorf("otp stats: %w", err)
	}
	return last, lastHour, nil
}

// LockLatestChallenge locks the most recent challenge for phone (FOR UPDATE) so that
// concurrent verifications are serialized.
func (r *Repository) LockLatestChallenge(ctx context.Context, tx pgx.Tx, phone string) (*Challenge, error) {
	var c Challenge
	err := tx.QueryRow(ctx, `
		SELECT id, phone_e164, code_hash, attempts, max_attempts, expires_at, consumed_at, created_at
		FROM otp_challenges WHERE phone_e164 = $1
		ORDER BY created_at DESC LIMIT 1
		FOR UPDATE`, phone).
		Scan(&c.ID, &c.PhoneE164, &c.CodeHash, &c.Attempts, &c.MaxAttempts, &c.ExpiresAt, &c.ConsumedAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock otp challenge: %w", err)
	}
	return &c, nil
}

func (r *Repository) IncrementAttempts(ctx context.Context, q postgres.DBTX, id uuid.UUID) (int, error) {
	var attempts int
	err := q.QueryRow(ctx, `UPDATE otp_challenges SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts`, id).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("increment otp attempts: %w", err)
	}
	return attempts, nil
}

func (r *Repository) ConsumeChallenge(ctx context.Context, q postgres.DBTX, id uuid.UUID) error {
	if _, err := q.Exec(ctx, `UPDATE otp_challenges SET consumed_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("consume otp challenge: %w", err)
	}
	return nil
}

func (r *Repository) CreateSession(ctx context.Context, q postgres.DBTX, userID uuid.UUID, refreshHash []byte, ttl time.Duration, userAgent string, ip *netip.Addr) (Session, error) {
	s := Session{UserID: userID}
	err := q.QueryRow(ctx, `
		INSERT INTO sessions (user_id, refresh_token_hash, expires_at, user_agent, ip)
		VALUES ($1, $2, now() + make_interval(secs => $3), NULLIF(left($4, 512), ''), $5)
		RETURNING id, expires_at`,
		userID, refreshHash, ttl.Seconds(), userAgent, ip).Scan(&s.ID, &s.ExpiresAt)
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return s, nil
}

type refreshLookup struct {
	Session
	MatchedPrevious bool
}

// LockSessionByRefreshHash finds the session whose current or previous refresh hash
// matches, locking the row.
func (r *Repository) LockSessionByRefreshHash(ctx context.Context, tx pgx.Tx, hash []byte) (*refreshLookup, error) {
	var l refreshLookup
	err := tx.QueryRow(ctx, `
		SELECT id, user_id, expires_at, revoked_at, revoked_reason, refresh_token_hash <> $1
		FROM sessions
		WHERE refresh_token_hash = $1 OR previous_refresh_token_hash = $1
		LIMIT 1
		FOR UPDATE`, hash).
		Scan(&l.ID, &l.UserID, &l.ExpiresAt, &l.RevokedAt, &l.RevokedReason, &l.MatchedPrevious)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup session: %w", err)
	}
	return &l, nil
}

func (r *Repository) RotateRefresh(ctx context.Context, q postgres.DBTX, sessionID uuid.UUID, newHash []byte) error {
	_, err := q.Exec(ctx, `
		UPDATE sessions
		SET previous_refresh_token_hash = refresh_token_hash, refresh_token_hash = $2, last_used_at = now()
		WHERE id = $1`, sessionID, newHash)
	if err != nil {
		return fmt.Errorf("rotate refresh token: %w", err)
	}
	return nil
}

func (r *Repository) RevokeSession(ctx context.Context, q postgres.DBTX, sessionID uuid.UUID, reason string) error {
	_, err := q.Exec(ctx, `
		UPDATE sessions SET revoked_at = now(), revoked_reason = $2
		WHERE id = $1 AND revoked_at IS NULL`, sessionID, reason)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeAllSessions revokes every active session of a user and returns their ids.
func (r *Repository) RevokeAllSessions(ctx context.Context, q postgres.DBTX, userID uuid.UUID, reason string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		UPDATE sessions SET revoked_at = now(), revoked_reason = $2
		WHERE user_id = $1 AND revoked_at IS NULL
		RETURNING id`, userID, reason)
	if err != nil {
		return nil, fmt.Errorf("revoke sessions: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// SessionActive reports whether the session exists, is not revoked and has not expired.
func (r *Repository) SessionActive(ctx context.Context, q postgres.DBTX, sessionID uuid.UUID) (bool, error) {
	var active bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND revoked_at IS NULL AND expires_at > now())`,
		sessionID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check session: %w", err)
	}
	return active, nil
}

// PurgeExpired deletes OTP challenges older than a day and sessions that expired or
// were revoked more than retention ago. These are auth artefacts, not user data.
func (r *Repository) PurgeExpired(ctx context.Context, q postgres.DBTX, retention time.Duration) (otps, sessions int64, err error) {
	tag, err := q.Exec(ctx, `DELETE FROM otp_challenges WHERE created_at < now() - interval '1 day'`)
	if err != nil {
		return 0, 0, fmt.Errorf("purge otp challenges: %w", err)
	}
	otps = tag.RowsAffected()
	tag, err = q.Exec(ctx, `
		DELETE FROM sessions
		WHERE expires_at < now() - make_interval(secs => $1)
		   OR revoked_at < now() - make_interval(secs => $1)`, retention.Seconds())
	if err != nil {
		return otps, 0, fmt.Errorf("purge sessions: %w", err)
	}
	return otps, tag.RowsAffected(), nil
}
