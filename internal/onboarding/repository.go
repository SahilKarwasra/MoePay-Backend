package onboarding

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
)

type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

// Init creates the progress row if missing (new signup or migrated user).
func (r *Repository) Init(ctx context.Context, q postgres.DBTX, userID uuid.UUID) error {
	_, err := q.Exec(ctx, `INSERT INTO onboarding_progress (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, userID)
	if err != nil {
		return fmt.Errorf("init onboarding: %w", err)
	}
	return nil
}

func (r *Repository) get(ctx context.Context, q postgres.DBTX, userID uuid.UUID, lock bool) (Progress, error) {
	query := `SELECT status, current_step, completed_steps, completed_at, version, updated_at
		FROM onboarding_progress WHERE user_id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	var p Progress
	var steps []string
	var current string
	err := q.QueryRow(ctx, query, userID).Scan(&p.Status, &current, &steps, &p.CompletedAt, &p.Version, &p.UpdatedAt)
	if err != nil {
		return Progress{}, err
	}
	p.CurrentStep = Step(current)
	for _, s := range steps {
		p.CompletedSteps = append(p.CompletedSteps, Step(s))
	}
	return p, nil
}

// Get returns the progress row, creating it if it does not exist yet.
func (r *Repository) Get(ctx context.Context, q postgres.DBTX, userID uuid.UUID, forUpdate bool) (Progress, error) {
	p, err := r.get(ctx, q, userID, forUpdate)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := r.Init(ctx, q, userID); err != nil {
			return Progress{}, err
		}
		p, err = r.get(ctx, q, userID, forUpdate)
	}
	if err != nil {
		return Progress{}, fmt.Errorf("get onboarding: %w", err)
	}
	return p, nil
}

// Save persists the derived step state, and completion when complete is true.
func (r *Repository) Save(ctx context.Context, q postgres.DBTX, userID uuid.UUID, current Step, done []Step, complete bool) (Progress, error) {
	steps := make([]string, len(done))
	for i, s := range done {
		steps[i] = string(s)
	}
	status := StatusInProgress
	if complete {
		status = StatusCompleted
	}
	_, err := q.Exec(ctx, `
		UPDATE onboarding_progress
		SET status = $2,
		    current_step = $3,
		    completed_steps = $4,
		    completed_at = CASE WHEN $2 = 'completed' THEN COALESCE(completed_at, now()) END,
		    version = version + 1
		WHERE user_id = $1`, userID, status, string(current), steps)
	if err != nil {
		return Progress{}, fmt.Errorf("save onboarding: %w", err)
	}
	return r.get(ctx, q, userID, false)
}
