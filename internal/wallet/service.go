package wallet

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/auth"
	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/internal/money"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
	"github.com/sahilkarwasra/moepay/internal/user"
)

// OTPService confirms sensitive actions with a one-time code (implemented by auth.Service).
type OTPService interface {
	SendActionOTP(ctx context.Context, userID uuid.UUID, purpose string, meta auth.ClientMeta) (auth.SendOTPResult, error)
	VerifyActionOTP(ctx context.Context, userID uuid.UUID, purpose, code string) error
}

type Service struct {
	pool  *pgxpool.Pool
	repo  *Repository
	users *user.Repository
	otp   OTPService
	cfg   config.WalletConfig
	now   func() time.Time
}

func NewService(pool *pgxpool.Pool, repo *Repository, users *user.Repository, otp OTPService, cfg config.WalletConfig) *Service {
	return &Service{pool: pool, repo: repo, users: users, otp: otp, cfg: cfg, now: time.Now}
}

// OnOnboardingCompleted opens the user's wallet inside the onboarding transaction.
func (s *Service) OnOnboardingCompleted(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	return s.repo.Create(ctx, tx, userID)
}

// ForUser returns the user's wallet, creating it for users who completed onboarding
// before wallets existed. Users still onboarding get WALLET_NOT_READY.
func (s *Service) ForUser(ctx context.Context, q postgres.DBTX, userID uuid.UUID) (Wallet, error) {
	w, err := s.repo.GetByUser(ctx, q, userID)
	if !errors.Is(err, errNotFound) {
		return w, err
	}
	done, err := s.repo.onboardingCompleted(ctx, q, userID)
	if err != nil {
		return Wallet{}, err
	}
	if !done {
		return Wallet{}, apperr.ErrWalletNotReady
	}
	if err := s.repo.Create(ctx, q, userID); err != nil {
		return Wallet{}, err
	}
	return s.repo.GetByUser(ctx, q, userID)
}

// PINStatus is shown on the wallet screen.
type PINStatus struct {
	Set               bool       `json:"set"`
	Locked            bool       `json:"locked"`
	LockedUntil       *time.Time `json:"locked_until"`
	AttemptsRemaining int        `json:"attempts_remaining"`
}

func (s *Service) PINStatus(w Wallet) PINStatus {
	st := PINStatus{Set: w.PINSet, AttemptsRemaining: max(s.cfg.PINMaxAttempts-w.PINFailedAttempts, 0)}
	if w.PINLockedUntil != nil && s.now().Before(*w.PINLockedUntil) {
		st.Locked, st.LockedUntil, st.AttemptsRemaining = true, w.PINLockedUntil, 0
	}
	return st
}

type SetPINInput struct {
	PIN        string `json:"pin"`
	ConfirmPIN string `json:"confirm_pin"`
}

func (s *Service) SetPIN(ctx context.Context, userID uuid.UUID, in SetPINInput) (PINStatus, error) {
	if err := ValidatePIN(in.PIN); err != nil {
		return PINStatus{}, err
	}
	if in.ConfirmPIN != in.PIN {
		return PINStatus{}, apperr.Validation("PINs do not match", map[string]string{"confirm_pin": "must match pin"})
	}
	hash, err := hashPIN(in.PIN)
	if err != nil {
		return PINStatus{}, err
	}
	return s.withPIN(ctx, userID, func(tx pgx.Tx, st pinState) error {
		if st.hash != nil {
			return apperr.ErrPINAlreadySet
		}
		return s.repo.setPIN(ctx, tx, st.walletID, hash)
	})
}

type ChangePINInput struct {
	CurrentPIN string `json:"current_pin"`
	NewPIN     string `json:"new_pin"`
}

func (s *Service) ChangePIN(ctx context.Context, userID uuid.UUID, in ChangePINInput) (PINStatus, error) {
	if err := ValidatePIN(in.NewPIN); err != nil {
		return PINStatus{}, err
	}
	if in.NewPIN == in.CurrentPIN {
		return PINStatus{}, apperr.Validation("New PIN must be different", map[string]string{"new_pin": "must differ from current_pin"})
	}
	if err := s.VerifyPIN(ctx, userID, in.CurrentPIN); err != nil {
		return PINStatus{}, err
	}
	hash, err := hashPIN(in.NewPIN)
	if err != nil {
		return PINStatus{}, err
	}
	return s.withPIN(ctx, userID, func(tx pgx.Tx, st pinState) error {
		return s.repo.setPIN(ctx, tx, st.walletID, hash)
	})
}

func (s *Service) SendPINResetOTP(ctx context.Context, userID uuid.UUID, meta auth.ClientMeta) (auth.SendOTPResult, error) {
	if _, err := s.ForUser(ctx, s.pool, userID); err != nil {
		return auth.SendOTPResult{}, err
	}
	return s.otp.SendActionOTP(ctx, userID, auth.PurposePINReset, meta)
}

type ResetPINInput struct {
	OTP    string `json:"otp"`
	NewPIN string `json:"new_pin"`
}

// ResetPIN sets a new PIN after verifying an OTP sent to the user's phone. It also
// clears any lockout.
func (s *Service) ResetPIN(ctx context.Context, userID uuid.UUID, in ResetPINInput) (PINStatus, error) {
	if err := ValidatePIN(in.NewPIN); err != nil {
		return PINStatus{}, err
	}
	if _, err := s.ForUser(ctx, s.pool, userID); err != nil {
		return PINStatus{}, err
	}
	if err := s.otp.VerifyActionOTP(ctx, userID, auth.PurposePINReset, strings.TrimSpace(in.OTP)); err != nil {
		return PINStatus{}, err
	}
	hash, err := hashPIN(in.NewPIN)
	if err != nil {
		return PINStatus{}, err
	}
	return s.withPIN(ctx, userID, func(tx pgx.Tx, st pinState) error {
		return s.repo.setPIN(ctx, tx, st.walletID, hash)
	})
}

// VerifyPIN checks pin and enforces the lockout. Failed attempts are committed even
// though an error is returned, so they cannot be undone by aborting the request.
func (s *Service) VerifyPIN(ctx context.Context, userID uuid.UUID, pin string) error {
	if len(pin) != pinLength {
		return apperr.ErrPINFormat
	}
	if _, err := s.ForUser(ctx, s.pool, userID); err != nil {
		return err
	}
	var failure error
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		st, err := s.repo.lockPIN(ctx, tx, userID)
		if err != nil {
			return err
		}
		now := s.now()
		switch {
		case st.hash == nil:
			failure = apperr.ErrPINNotSet
			return nil
		case st.lockedUntil != nil && now.Before(*st.lockedUntil):
			failure = lockedErr(*st.lockedUntil, now)
			return nil
		}
		ok, err := verifyPIN(pin, *st.hash)
		if err != nil {
			return err
		}
		if ok {
			return s.repo.clearPINFailures(ctx, tx, st.walletID)
		}
		attempts := st.attempts + 1
		if st.lockedUntil != nil { // previous lock has expired: start counting afresh
			attempts = 1
		}
		var until *time.Time
		if attempts >= s.cfg.PINMaxAttempts {
			t := now.Add(s.cfg.PINLockDuration)
			until, attempts = &t, 0
			failure = lockedErr(t, now)
		} else {
			failure = apperr.ErrPINInvalid.WithDetails(map[string]any{"attempts_remaining": s.cfg.PINMaxAttempts - attempts})
		}
		return s.repo.recordPINFailure(ctx, tx, st.walletID, attempts, until)
	})
	if err != nil {
		return err
	}
	return failure
}

func lockedErr(until, now time.Time) error {
	return apperr.ErrPINLocked.WithDetails(map[string]any{
		"locked_until":        until.UTC(),
		"retry_after_seconds": int(until.Sub(now).Seconds()) + 1,
	})
}

func (s *Service) withPIN(ctx context.Context, userID uuid.UUID, fn func(pgx.Tx, pinState) error) (PINStatus, error) {
	if _, err := s.ForUser(ctx, s.pool, userID); err != nil {
		return PINStatus{}, err
	}
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		st, err := s.repo.lockPIN(ctx, tx, userID)
		if err != nil {
			return err
		}
		return fn(tx, st)
	})
	if err != nil {
		return PINStatus{}, err
	}
	w, err := s.repo.GetByUser(ctx, s.pool, userID)
	if err != nil {
		return PINStatus{}, err
	}
	return s.PINStatus(w), nil
}

// QR is the payload the app renders as a "pay me" QR code.
type QR struct {
	Payload     string        `json:"payload"`
	Username    string        `json:"username"`
	DisplayName string        `json:"display_name"`
	Amount      *money.Amount `json:"amount"`
	Note        *string       `json:"note"`
}

const QRScheme = "moepay"

func (s *Service) QR(ctx context.Context, userID uuid.UUID, amountPence int64, note string) (QR, error) {
	if _, err := s.ForUser(ctx, s.pool, userID); err != nil {
		return QR{}, err
	}
	u, err := s.users.GetByID(ctx, s.pool, userID)
	if err != nil {
		return QR{}, err
	}
	p, err := s.users.GetProfile(ctx, s.pool, userID)
	if err != nil {
		return QR{}, err
	}
	if u.Username == nil {
		return QR{}, apperr.ErrWalletNotReady
	}
	q := url.Values{"u": {*u.Username}}
	out := QR{Username: *u.Username, DisplayName: user.DisplayName(u, p)}
	if amountPence != 0 {
		if amountPence < s.cfg.MinPerTxnPence || amountPence > s.cfg.MaxPerTxnPence {
			return QR{}, apperr.ErrAmountInvalid
		}
		a := money.New(amountPence)
		out.Amount = &a
		q.Set("a", strconv.FormatInt(amountPence, 10))
	}
	if note = strings.TrimSpace(note); note != "" {
		if len([]rune(note)) > 140 {
			return QR{}, apperr.Validation("Note is too long", map[string]string{"note": "at most 140 characters"})
		}
		out.Note = &note
		q.Set("n", note)
	}
	out.Payload = (&url.URL{Scheme: QRScheme, Host: "pay", RawQuery: q.Encode()}).String()
	return out, nil
}
