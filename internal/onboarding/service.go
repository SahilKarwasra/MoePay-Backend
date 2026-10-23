package onboarding

import (
	"context"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
	"github.com/sahilkarwasra/moepay/internal/user"
	"github.com/sahilkarwasra/moepay/internal/username"
)

// Publisher pushes realtime events to a user's connected clients.
type Publisher interface {
	PublishToUser(ctx context.Context, userID uuid.UUID, eventType string, data any)
}

// CompletionHook runs inside the transaction that completes onboarding (e.g. to open
// the user's wallet), so completion and its side effects commit together.
type CompletionHook func(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error

type Service struct {
	hooks  []CompletionHook
	pool   *pgxpool.Pool
	repo   *Repository
	users  *user.Repository
	names  *username.Checker
	events Publisher
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, repo *Repository, users *user.Repository, names *username.Checker, events Publisher) *Service {
	return &Service{pool: pool, repo: repo, users: users, names: names, events: events, now: time.Now}
}

// OnComplete registers a hook that runs when a user completes onboarding.
func (s *Service) OnComplete(h CompletionHook) { s.hooks = append(s.hooks, h) }

// Init creates progress for a new user inside the caller's transaction.
func (s *Service) Init(ctx context.Context, q postgres.DBTX, userID uuid.UUID) error {
	return s.repo.Init(ctx, q, userID)
}

// Summary returns status and next step inside the caller's transaction.
func (s *Service) Summary(ctx context.Context, q postgres.DBTX, u user.User) (Summary, error) {
	prog, err := s.repo.Get(ctx, q, u.ID, false)
	if err != nil {
		return Summary{}, err
	}
	p, err := s.users.GetProfile(ctx, q, u.ID)
	if err != nil {
		return Summary{}, err
	}
	_, next := derive(u, p, prog.Status == StatusCompleted)
	return Summary{Status: prog.Status, NextStep: next}, nil
}

func (s *Service) Get(ctx context.Context, userID uuid.UUID) (State, error) {
	u, err := s.users.GetByID(ctx, s.pool, userID)
	if err != nil {
		return State{}, err
	}
	p, err := s.users.GetProfile(ctx, s.pool, userID)
	if err != nil {
		return State{}, err
	}
	prog, err := s.repo.Get(ctx, s.pool, userID, false)
	if err != nil {
		return State{}, err
	}
	return buildState(u, p, prog), nil
}

type ProfileInput struct {
	FirstName   string  `json:"first_name"`
	LastName    string  `json:"last_name"`
	Email       *string `json:"email"`
	DateOfBirth *string `json:"date_of_birth"`
}

func (s *Service) SaveProfile(ctx context.Context, userID uuid.UUID, in ProfileInput) (State, error) {
	profile, err := s.validateProfile(in)
	if err != nil {
		return State{}, err
	}
	return s.mutate(ctx, userID, StepProfile, func(tx pgx.Tx, _ user.User) error {
		return s.users.SaveProfile(ctx, tx, userID, profile)
	}, nil)
}

type UsernameInput struct {
	Username string `json:"username"`
}

func (s *Service) SaveUsername(ctx context.Context, userID uuid.UUID, in UsernameInput) (State, error) {
	display, normalized, err := username.Normalize(in.Username)
	if err != nil {
		return State{}, err
	}
	if username.IsReserved(normalized) {
		return State{}, apperr.ErrUsernameTaken
	}
	return s.mutate(ctx, userID, StepUsername, func(tx pgx.Tx, u user.User) error {
		if u.Username != nil && *u.Username == display {
			return nil
		}
		return s.users.SetUsername(ctx, tx, userID, display, normalized)
	}, func() { s.names.Added(ctx, normalized) })
}

// Complete validates every required step and marks onboarding completed. It is
// idempotent so a client retrying after a network error gets the same result.
func (s *Service) Complete(ctx context.Context, userID uuid.UUID) (State, error) {
	var state State
	var justCompleted bool
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		prog, err := s.repo.Get(ctx, tx, userID, true)
		if err != nil {
			return err
		}
		u, p, err := s.load(ctx, tx, userID)
		if err != nil {
			return err
		}
		if prog.Status == StatusCompleted {
			state = buildState(u, p, prog)
			return nil
		}
		done, next := derive(u, p, false)
		if next != StepReview {
			missing := []Step{}
			for _, st := range RequiredSteps {
				if !satisfied(u, p)[st] {
					missing = append(missing, st)
				}
			}
			return apperr.ErrOnboardingIncomplete.WithDetails(map[string]any{"missing_steps": missing, "next_step": next})
		}
		prog, err = s.repo.Save(ctx, tx, userID, StepCompleted, done, true)
		if err != nil {
			return err
		}
		for _, h := range s.hooks {
			if err := h(ctx, tx, userID); err != nil {
				return err
			}
		}
		state, justCompleted = buildState(u, p, prog), true
		return nil
	})
	if err != nil {
		return State{}, err
	}
	if justCompleted {
		s.events.PublishToUser(ctx, userID, "onboarding.completed", state)
	}
	return state, nil
}

// mutate runs a step update in one transaction: lock progress, enforce transitions,
// apply the change, re-derive and persist the step state.
func (s *Service) mutate(ctx context.Context, userID uuid.UUID, step Step, apply func(pgx.Tx, user.User) error, afterCommit func()) (State, error) {
	var state State
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		prog, err := s.repo.Get(ctx, tx, userID, true)
		if err != nil {
			return err
		}
		if prog.Status == StatusCompleted {
			return apperr.ErrOnboardingDone
		}
		u, p, err := s.load(ctx, tx, userID)
		if err != nil {
			return err
		}
		if u.Status != user.StatusActive {
			return apperr.ErrUserInactive
		}
		if !canSave(step, u, p) {
			_, next := derive(u, p, false)
			return apperr.ErrStepOutOfOrder.WithDetails(map[string]any{"next_step": next})
		}
		if err := apply(tx, u); err != nil {
			return err
		}
		if u, p, err = s.load(ctx, tx, userID); err != nil {
			return err
		}
		done, next := derive(u, p, false)
		if prog, err = s.repo.Save(ctx, tx, userID, next, done, false); err != nil {
			return err
		}
		state = buildState(u, p, prog)
		return nil
	})
	if err != nil {
		return State{}, err
	}
	if afterCommit != nil {
		afterCommit()
	}
	s.events.PublishToUser(ctx, userID, "onboarding.updated", state)
	return state, nil
}

func (s *Service) load(ctx context.Context, q postgres.DBTX, userID uuid.UUID) (user.User, user.Profile, error) {
	u, err := s.users.GetByID(ctx, q, userID)
	if err != nil {
		return user.User{}, user.Profile{}, err
	}
	p, err := s.users.GetProfile(ctx, q, userID)
	return u, p, err
}

func (s *Service) validateProfile(in ProfileInput) (user.Profile, error) {
	fields := map[string]string{}
	first, ok := cleanName(in.FirstName)
	if !ok {
		fields["first_name"] = "required, 1-50 characters, letters, spaces, '.', '-' or apostrophe"
	}
	last, ok := cleanName(in.LastName)
	if !ok {
		fields["last_name"] = "required, 1-50 characters, letters, spaces, '.', '-' or apostrophe"
	}
	out := user.Profile{FirstName: &first, LastName: &last}

	if in.Email != nil && strings.TrimSpace(*in.Email) != "" {
		email := strings.ToLower(strings.TrimSpace(*in.Email))
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address != email || len(email) > 254 || !strings.Contains(email[strings.LastIndex(email, "@"):], ".") {
			fields["email"] = "must be a valid email address"
		}
		out.Email = &email
	}
	if in.DateOfBirth != nil && strings.TrimSpace(*in.DateOfBirth) != "" {
		dob, err := time.Parse(time.DateOnly, strings.TrimSpace(*in.DateOfBirth))
		if err != nil || dob.Year() < 1900 || dob.After(s.now()) {
			fields["date_of_birth"] = "must be a past date in YYYY-MM-DD format"
		}
		out.DateOfBirth = &dob
	}
	if len(fields) > 0 {
		return user.Profile{}, apperr.Validation("Profile is not valid", fields)
	}
	return out, nil
}

func cleanName(s string) (string, bool) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || utf8.RuneCountInString(s) > 50 {
		return "", false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsMark(r) && r != ' ' && r != '.' && r != '-' && r != '\'' {
			return "", false
		}
	}
	return s, true
}
