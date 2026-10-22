package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/internal/onboarding"
	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
	"github.com/sahilkarwasra/moepay/internal/ratelimit"
	"github.com/sahilkarwasra/moepay/internal/user"
)

// SessionEvents is notified when sessions are revoked so live connections close.
type SessionEvents interface {
	SessionsRevoked(ctx context.Context, userID uuid.UUID, sessionIDs []uuid.UUID)
}

type Service struct {
	cfg        config.AuthConfig
	pool       *pgxpool.Pool
	repo       *Repository
	users      *user.Repository
	onboarding *onboarding.Service
	tokens     *TokenIssuer
	otp        *OTPCodec
	sms        SMSSender
	limiter    *ratelimit.Limiter
	revoked    *RevocationCache
	events     SessionEvents
	logger     *slog.Logger
}

type Deps struct {
	Config     config.AuthConfig
	Pool       *pgxpool.Pool
	Repo       *Repository
	Users      *user.Repository
	Onboarding *onboarding.Service
	Tokens     *TokenIssuer
	OTP        *OTPCodec
	SMS        SMSSender
	Limiter    *ratelimit.Limiter
	Revoked    *RevocationCache
	Events     SessionEvents
	Logger     *slog.Logger
}

func NewService(d Deps) *Service {
	return &Service{
		cfg: d.Config, pool: d.Pool, repo: d.Repo, users: d.Users, onboarding: d.Onboarding,
		tokens: d.Tokens, otp: d.OTP, sms: d.SMS, limiter: d.Limiter, revoked: d.Revoked,
		events: d.Events, logger: d.Logger,
	}
}

// ClientMeta describes the calling device.
type ClientMeta struct {
	IP        *netip.Addr
	UserAgent string
}

func (m ClientMeta) ipKey() string {
	if m.IP == nil {
		return "unknown"
	}
	return m.IP.String()
}

type SendOTPResult struct {
	PhoneNumber        string `json:"phone_number"`
	ExpiresInSeconds   int    `json:"expires_in_seconds"`
	ResendAfterSeconds int    `json:"resend_after_seconds"`
}

func (s *Service) SendOTP(ctx context.Context, rawPhone string, meta ClientMeta) (SendOTPResult, error) {
	phone, err := NormalizePhone(rawPhone, s.cfg.DefaultRegion)
	if err != nil {
		return SendOTPResult{}, err
	}

	// Redis limits (shared across instances): resend cooldown, per phone, per IP.
	if err := s.limit(ctx, ratelimit.Rule{Name: "otp_cooldown", Limit: 1, Window: s.cfg.OTPResendCooldown}, phone, apperr.ErrOTPCooldown); err != nil {
		return SendOTPResult{}, err
	}
	if err := s.limit(ctx, ratelimit.Rule{Name: "otp_phone", Limit: s.cfg.OTPMaxPerPhoneHour, Window: time.Hour}, phone, apperr.ErrRateLimited); err != nil {
		return SendOTPResult{}, err
	}
	if err := s.limit(ctx, ratelimit.Rule{Name: "otp_ip", Limit: s.cfg.OTPMaxPerIPHour, Window: time.Hour}, meta.ipKey(), apperr.ErrRateLimited); err != nil {
		return SendOTPResult{}, err
	}

	// Database guard, independent of Redis availability.
	last, lastHour, err := s.repo.RecentChallengeStats(ctx, s.pool, phone)
	if err != nil {
		return SendOTPResult{}, err
	}
	if last != nil {
		if wait := s.cfg.OTPResendCooldown - time.Since(*last); wait > 0 {
			return SendOTPResult{}, retryAfter(apperr.ErrOTPCooldown, wait)
		}
	}
	if lastHour >= s.cfg.OTPMaxPerPhoneHour {
		return SendOTPResult{}, retryAfter(apperr.ErrRateLimited, time.Hour)
	}

	code, err := s.otp.Generate()
	if err != nil {
		return SendOTPResult{}, apperr.ErrUnavailable.Wrap(err)
	}
	if err := s.repo.CreateChallenge(ctx, s.pool, phone, s.otp.Hash(phone, code), s.cfg.OTPMaxAttempts, s.cfg.OTPTTL, meta.IP); err != nil {
		return SendOTPResult{}, err
	}
	if err := s.sms.SendOTP(ctx, phone, code); err != nil {
		s.logger.ErrorContext(ctx, "sms delivery failed", "phone", MaskPhone(phone), "err", err)
		return SendOTPResult{}, apperr.ErrUnavailable.Wrap(err)
	}
	s.logger.InfoContext(ctx, "otp sent", "phone", MaskPhone(phone))
	return SendOTPResult{
		PhoneNumber:        phone,
		ExpiresInSeconds:   int(s.cfg.OTPTTL.Seconds()),
		ResendAfterSeconds: int(s.cfg.OTPResendCooldown.Seconds()),
	}, nil
}

type TokenPair struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	TokenType        string    `json:"token_type"`
	ExpiresIn        int       `json:"expires_in"`
	AccessExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_token_expires_at"`
}

type LoginResult struct {
	TokenPair
	IsUserNew  bool               `json:"is_user_new"`
	User       user.View          `json:"user"`
	Onboarding onboarding.Summary `json:"onboarding"`
}

func (s *Service) VerifyOTP(ctx context.Context, rawPhone, code string, meta ClientMeta) (LoginResult, error) {
	phone, err := NormalizePhone(rawPhone, s.cfg.DefaultRegion)
	if err != nil {
		return LoginResult{}, err
	}
	if !isDigits(code, s.otp.Length()) {
		return LoginResult{}, apperr.ErrOTPInvalid
	}
	if err := s.limit(ctx, ratelimit.Rule{Name: "otp_verify_phone", Limit: s.cfg.VerifyMaxPerPhone, Window: 15 * time.Minute}, phone, apperr.ErrRateLimited); err != nil {
		return LoginResult{}, err
	}
	if err := s.limit(ctx, ratelimit.Rule{Name: "otp_verify_ip", Limit: s.cfg.VerifyMaxPerPhone * 5, Window: 15 * time.Minute}, meta.ipKey(), apperr.ErrRateLimited); err != nil {
		return LoginResult{}, err
	}

	var (
		result LoginResult
		// failure is committed (e.g. an incremented attempt counter) and then returned.
		failure error
	)
	err = postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		ch, err := s.repo.LockLatestChallenge(ctx, tx, phone)
		if err != nil {
			return err
		}
		if ch == nil || ch.ConsumedAt != nil || !time.Now().Before(ch.ExpiresAt) {
			failure = apperr.ErrOTPExpired
			return nil
		}
		if ch.Attempts >= ch.MaxAttempts {
			failure = apperr.ErrOTPExhausted
			return nil
		}
		if !s.otp.Verify(phone, code, ch.CodeHash) {
			attempts, err := s.repo.IncrementAttempts(ctx, tx, ch.ID)
			if err != nil {
				return err
			}
			failure = apperr.ErrOTPInvalid.WithDetails(map[string]any{"attempts_remaining": max(ch.MaxAttempts-attempts, 0)})
			return nil
		}
		if err := s.repo.ConsumeChallenge(ctx, tx, ch.ID); err != nil {
			return err
		}

		u, created, err := s.users.GetOrCreateByPhone(ctx, tx, phone)
		if err != nil {
			return err
		}
		if u.Status != user.StatusActive {
			failure = apperr.ErrUserInactive
			return nil
		}
		if created {
			if err := s.onboarding.Init(ctx, tx, u.ID); err != nil {
				return err
			}
		}
		pair, err := s.createSession(ctx, tx, u.ID, meta)
		if err != nil {
			return err
		}
		summary, err := s.onboarding.Summary(ctx, tx, u)
		if err != nil {
			return err
		}
		profile, err := s.users.GetProfile(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		result = LoginResult{
			TokenPair:  pair,
			IsUserNew:  summary.Status != onboarding.StatusCompleted,
			User:       user.NewView(u, profile),
			Onboarding: summary,
		}
		if created {
			s.logger.InfoContext(ctx, "user created", "user_id", u.ID)
		}
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}
	if failure != nil {
		return LoginResult{}, failure
	}
	return result, nil
}

func (s *Service) createSession(ctx context.Context, q postgres.DBTX, userID uuid.UUID, meta ClientMeta) (TokenPair, error) {
	refresh, hash, err := NewRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}
	sess, err := s.repo.CreateSession(ctx, q, userID, hash, s.cfg.RefreshTokenTTL, meta.UserAgent, meta.IP)
	if err != nil {
		return TokenPair{}, err
	}
	return s.pair(userID, sess, refresh)
}

func (s *Service) pair(userID uuid.UUID, sess Session, refresh string) (TokenPair, error) {
	access, exp, err := s.tokens.Issue(userID, sess.ID)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:      access,
		RefreshToken:     refresh,
		TokenType:        "Bearer",
		ExpiresIn:        int(s.tokens.TTL().Seconds()),
		AccessExpiresAt:  exp,
		RefreshExpiresAt: sess.ExpiresAt,
	}, nil
}

// Refresh rotates the refresh token. Presenting an already-rotated token is treated
// as theft: the whole session is revoked (OAuth 2.0 Security BCP §4.14).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	if refreshToken == "" || len(refreshToken) > 128 {
		return TokenPair{}, apperr.ErrTokenInvalid
	}
	hash := HashRefreshToken(refreshToken)
	var (
		pair    TokenPair
		failure error
		reused  *refreshLookup
	)
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		l, err := s.repo.LockSessionByRefreshHash(ctx, tx, hash)
		if err != nil {
			return err
		}
		switch {
		case l == nil:
			failure = apperr.ErrTokenInvalid
			return nil
		case l.RevokedAt != nil:
			failure = apperr.ErrSessionRevoked
			return nil
		case l.MatchedPrevious:
			if err := s.repo.RevokeSession(ctx, tx, l.ID, "refresh_reuse"); err != nil {
				return err
			}
			reused, failure = l, apperr.ErrSessionRevoked
			return nil
		case !time.Now().Before(l.ExpiresAt):
			failure = apperr.ErrTokenInvalid
			return nil
		}
		u, err := s.users.GetByID(ctx, tx, l.UserID)
		if err != nil {
			return err
		}
		if u.Status != user.StatusActive {
			failure = apperr.ErrUserInactive
			return nil
		}
		next, nextHash, err := NewRefreshToken()
		if err != nil {
			return err
		}
		if err := s.repo.RotateRefresh(ctx, tx, l.ID, nextHash); err != nil {
			return err
		}
		pair, err = s.pair(l.UserID, l.Session, next)
		return err
	})
	if err != nil {
		return TokenPair{}, err
	}
	if reused != nil {
		s.logger.WarnContext(ctx, "refresh token reuse detected; session revoked", "user_id", reused.UserID, "session_id", reused.ID)
		s.afterRevoke(ctx, reused.UserID, []uuid.UUID{reused.ID})
	}
	if failure != nil {
		return TokenPair{}, failure
	}
	return pair, nil
}

func (s *Service) Logout(ctx context.Context, p httpx.Principal) error {
	if err := s.repo.RevokeSession(ctx, s.pool, p.SessionID, "logout"); err != nil {
		return err
	}
	s.afterRevoke(ctx, p.UserID, []uuid.UUID{p.SessionID})
	return nil
}

func (s *Service) LogoutAll(ctx context.Context, p httpx.Principal) (int, error) {
	ids, err := s.repo.RevokeAllSessions(ctx, s.pool, p.UserID, "logout_all")
	if err != nil {
		return 0, err
	}
	s.afterRevoke(ctx, p.UserID, ids)
	return len(ids), nil
}

func (s *Service) afterRevoke(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) {
	if err := s.revoked.MarkRevoked(ctx, ids...); err != nil {
		// The database is authoritative; access tokens are short-lived. Log loudly.
		s.logger.ErrorContext(ctx, "failed to cache session revocation", "err", err)
	}
	s.events.SessionsRevoked(ctx, userID, ids)
}

// Authenticate validates an access token and checks its session is still active.
func (s *Service) Authenticate(ctx context.Context, accessToken string) (httpx.Principal, time.Time, error) {
	userID, sessionID, exp, err := s.tokens.Parse(accessToken)
	if err != nil {
		return httpx.Principal{}, time.Time{}, err
	}
	revoked, err := s.revoked.IsRevoked(ctx, sessionID)
	if err != nil {
		// Redis unavailable: fall back to the source of truth.
		active, dbErr := s.repo.SessionActive(ctx, s.pool, sessionID)
		if dbErr != nil {
			return httpx.Principal{}, time.Time{}, errors.Join(err, dbErr)
		}
		revoked = !active
	}
	if revoked {
		return httpx.Principal{}, time.Time{}, apperr.ErrSessionRevoked
	}
	return httpx.Principal{UserID: userID, SessionID: sessionID}, exp, nil
}

// SessionActive checks the database (used for long-lived connections).
func (s *Service) SessionActive(ctx context.Context, sessionID uuid.UUID) (bool, error) {
	return s.repo.SessionActive(ctx, s.pool, sessionID)
}

func (s *Service) limit(ctx context.Context, rule ratelimit.Rule, key string, onLimit *apperr.Error) error {
	if res := s.limiter.Allow(ctx, rule, key); !res.Allowed {
		return retryAfter(onLimit, res.RetryAfter)
	}
	return nil
}

func retryAfter(e *apperr.Error, d time.Duration) error {
	return e.WithDetails(map[string]any{"retry_after_seconds": int(d.Seconds()) + 1})
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
