package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
)

// Header is the request header clients send with every money-moving request.
const Header = "Idempotency-Key"

// Retention is how long a key is remembered.
const Retention = 24 * time.Hour

// outcome is what gets stored and replayed.
type outcome struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *storedError    `json:"error,omitempty"`
}

type storedError struct {
	Status  int            `json:"status"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type persistent struct{ err *apperr.Error }

func (p *persistent) Error() string { return p.err.Error() }

// Persist marks a business failure whose side effects (such as a recorded failed
// transaction) must be committed, and whose response must be replayed on retry.
// Any other error rolls everything back, so a retry runs the operation again.
func Persist(err *apperr.Error) error { return &persistent{err: err} }

// ValidateKey checks the client-supplied key.
func ValidateKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return apperr.ErrIdempotencyRequired
	}
	if len(key) < 8 || len(key) > 100 {
		return apperr.ErrIdempotencyRequired.WithDetails(map[string]any{"reason": "key must be 8-100 characters; a UUID is recommended"})
	}
	return nil
}

// Run executes fn exactly once per (user, key). fn runs inside the same database
// transaction that records the key, so the money movement and its stored response
// commit together. A retry with the same key and request replays the stored result;
// the same key with a different request is rejected. A concurrent duplicate blocks on
// the key's row until the first request finishes, then replays its result.
func Run[T any](ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, key, endpoint string, request any,
	fn func(tx pgx.Tx) (T, error)) (result T, replayed bool, err error) {
	if err := ValidateKey(key); err != nil {
		return result, false, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return result, false, fmt.Errorf("hash request: %w", err)
	}
	sum := sha256.Sum256(append([]byte(endpoint+"\x00"), body...))
	hash := sum[:]

	var failure error
	err = postgres.WithTx(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (user_id, key, endpoint, request_hash, outcome)
			VALUES ($1, $2, $3, $4, '{}') ON CONFLICT (user_id, key) DO NOTHING`,
			userID, key, endpoint, hash)
		if err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		}
		if tag.RowsAffected() == 0 {
			replayed = true
			return replay(ctx, tx, userID, key, endpoint, hash, &result, &failure)
		}

		r, ferr := fn(tx)
		var p *persistent
		if ferr != nil && !errors.As(ferr, &p) {
			return ferr
		}
		var out outcome
		if p != nil {
			out.Error = &storedError{Status: p.err.Status, Code: p.err.Code, Message: p.err.Message, Details: p.err.Details}
			failure = p.err
		} else {
			if out.Result, err = json.Marshal(r); err != nil {
				return fmt.Errorf("encode idempotent result: %w", err)
			}
			result = r
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE idempotency_keys SET outcome = $3 WHERE user_id = $1 AND key = $2`,
			userID, key, string(encoded)); err != nil {
			return fmt.Errorf("store idempotent outcome: %w", err)
		}
		return nil
	})
	if err != nil {
		var zero T
		return zero, false, err
	}
	if failure != nil {
		var zero T
		return zero, replayed, failure
	}
	return result, replayed, nil
}

func replay[T any](ctx context.Context, tx pgx.Tx, userID uuid.UUID, key, endpoint string, hash []byte, result *T, failure *error) error {
	var storedEndpoint string
	var storedHash, raw []byte
	err := tx.QueryRow(ctx, `SELECT endpoint, request_hash, outcome FROM idempotency_keys WHERE user_id = $1 AND key = $2`,
		userID, key).Scan(&storedEndpoint, &storedHash, &raw)
	if err != nil {
		return fmt.Errorf("load idempotency key: %w", err)
	}
	if storedEndpoint != endpoint || !bytes.Equal(storedHash, hash) {
		return apperr.ErrIdempotencyReused
	}
	var out outcome
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode idempotent outcome: %w", err)
	}
	switch {
	case out.Error != nil:
		e := apperr.New(out.Error.Status, out.Error.Code, out.Error.Message)
		if out.Error.Details != nil {
			e = e.WithDetails(out.Error.Details)
		}
		*failure = e
	case out.Result != nil:
		if err := json.Unmarshal(out.Result, result); err != nil {
			return fmt.Errorf("decode idempotent result: %w", err)
		}
	default:
		return apperr.ErrIdempotencyInFlight
	}
	return nil
}

// Purge deletes keys older than Retention.
func Purge(ctx context.Context, q postgres.DBTX) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < now() - make_interval(secs => $1)`, Retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("purge idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}
