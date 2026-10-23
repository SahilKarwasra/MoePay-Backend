package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // embed zone data so WALLET_TIMEZONE works on any host

	"github.com/joho/godotenv"
)

type Env string

const (
	EnvDevelopment Env = "development"
	EnvTest        Env = "test"
	EnvProduction  Env = "production"
)

type Config struct {
	Env      Env
	LogLevel string
	HTTP     HTTPConfig
	Postgres PostgresConfig
	Redis    RedisConfig
	Auth     AuthConfig
	Username UsernameConfig
	WS       WSConfig
	Wallet   WalletConfig
}

type HTTPConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	// TrustedProxies are CIDRs/IPs whose X-Forwarded-For is honoured for client IP.
	TrustedProxies []string
	CORSOrigins    []string
}

type PostgresConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type AuthConfig struct {
	JWTSecret          []byte
	JWTIssuer          string
	AccessTokenTTL     time.Duration
	RefreshTokenTTL    time.Duration
	OTPSecret          []byte
	OTPLength          int
	OTPTTL             time.Duration
	OTPMaxAttempts     int
	OTPResendCooldown  time.Duration
	OTPMaxPerPhoneHour int
	OTPMaxPerIPHour    int
	VerifyMaxPerPhone  int
	DefaultRegion      string
	SMSProvider        string
}

type UsernameConfig struct {
	BloomBackend       string // "memory" or "redis"
	BloomExpectedItems uint
	BloomFPRate        float64
	BloomRebuildEvery  time.Duration
}

type WSConfig struct {
	AllowedOrigins     []string
	MaxConnsPerUser    int
	MaxConnLifetime    time.Duration
	TicketTTL          time.Duration
	MaxMessageBytes    int64
	PingInterval       time.Duration
	SendBufferMessages int
}

// WalletConfig holds money limits. All amounts are in pence (GBP).
type WalletConfig struct {
	MaxBalancePence    int64
	MinPerTxnPence     int64
	MaxPerTxnPence     int64
	DailyOutgoingPence int64
	// Location defines the calendar day used for daily limits.
	Location          *time.Location
	MaxBankAccounts   int
	PINMaxAttempts    int
	PINLockDuration   time.Duration
	MoneyRequestTTL   time.Duration
	AccountHashSecret []byte
}

const minSecretBytes = 32

// Load reads configuration from the environment (and an optional .env file).
func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	return FromEnv(os.Getenv)
}

// FromEnv builds a Config from a lookup function, which keeps it testable.
func FromEnv(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	env := Env(p.str("APP_ENV", string(EnvDevelopment)))

	cfg := Config{
		Env:      env,
		LogLevel: p.str("LOG_LEVEL", "info"),
		HTTP: HTTPConfig{
			Addr:            p.str("HTTP_ADDR", ":"+p.str("PORT", "8080")),
			ReadTimeout:     p.duration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:    p.duration("HTTP_WRITE_TIMEOUT", 15*time.Second),
			IdleTimeout:     p.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout: p.duration("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
			TrustedProxies:  p.list("HTTP_TRUSTED_PROXIES"),
			CORSOrigins:     p.list("HTTP_CORS_ORIGINS"),
		},
		Postgres: PostgresConfig{
			URL:             p.str("DATABASE_URL", "postgres://localhost:5432/moepay?sslmode=disable"),
			MaxConns:        int32(p.int("DATABASE_MAX_CONNS", 20)),
			MinConns:        int32(p.int("DATABASE_MIN_CONNS", 2)),
			MaxConnLifetime: p.duration("DATABASE_MAX_CONN_LIFETIME", time.Hour),
		},
		Redis: RedisConfig{
			Addr:     p.str("REDIS_ADDR", "localhost:6379"),
			Password: p.str("REDIS_PASSWORD", ""),
			DB:       p.int("REDIS_DB", 0),
		},
		Auth: AuthConfig{
			JWTSecret:          []byte(p.str("JWT_SECRET", "")),
			JWTIssuer:          p.str("JWT_ISSUER", "moepay"),
			AccessTokenTTL:     p.duration("ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTokenTTL:    p.duration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
			OTPSecret:          []byte(p.str("OTP_SECRET", "")),
			OTPLength:          p.int("OTP_LENGTH", 6),
			OTPTTL:             p.duration("OTP_TTL", 5*time.Minute),
			OTPMaxAttempts:     p.int("OTP_MAX_ATTEMPTS", 5),
			OTPResendCooldown:  p.duration("OTP_RESEND_COOLDOWN", 30*time.Second),
			OTPMaxPerPhoneHour: p.int("OTP_MAX_PER_PHONE_PER_HOUR", 5),
			OTPMaxPerIPHour:    p.int("OTP_MAX_PER_IP_PER_HOUR", 30),
			VerifyMaxPerPhone:  p.int("OTP_VERIFY_MAX_PER_PHONE_PER_15M", 10),
			DefaultRegion:      strings.ToUpper(p.str("PHONE_DEFAULT_REGION", "IN")),
			SMSProvider:        p.str("SMS_PROVIDER", "log"),
		},
		Username: UsernameConfig{
			BloomBackend:       p.str("USERNAME_BLOOM_BACKEND", "memory"),
			BloomExpectedItems: uint(p.int("USERNAME_BLOOM_EXPECTED_ITEMS", 1_000_000)),
			BloomFPRate:        p.float("USERNAME_BLOOM_FP_RATE", 0.01),
			BloomRebuildEvery:  p.duration("USERNAME_BLOOM_REBUILD_INTERVAL", 6*time.Hour),
		},
		WS: WSConfig{
			AllowedOrigins:     p.list("WS_ALLOWED_ORIGINS"),
			MaxConnsPerUser:    p.int("WS_MAX_CONNS_PER_USER", 5),
			MaxConnLifetime:    p.duration("WS_MAX_CONN_LIFETIME", time.Hour),
			TicketTTL:          p.duration("WS_TICKET_TTL", 30*time.Second),
			MaxMessageBytes:    int64(p.int("WS_MAX_MESSAGE_BYTES", 4096)),
			PingInterval:       p.duration("WS_PING_INTERVAL", 30*time.Second),
			SendBufferMessages: p.int("WS_SEND_BUFFER", 32),
		},
		Wallet: WalletConfig{
			MaxBalancePence:    int64(p.int("WALLET_MAX_BALANCE_PENCE", 1_000_000)),
			MinPerTxnPence:     int64(p.int("WALLET_MIN_PER_TXN_PENCE", 1)),
			MaxPerTxnPence:     int64(p.int("WALLET_MAX_PER_TXN_PENCE", 500_000)),
			DailyOutgoingPence: int64(p.int("WALLET_DAILY_OUTGOING_PENCE", 1_000_000)),
			Location:           p.location("WALLET_TIMEZONE", "Europe/London"),
			MaxBankAccounts:    p.int("WALLET_MAX_BANK_ACCOUNTS", 5),
			PINMaxAttempts:     p.int("WALLET_PIN_MAX_ATTEMPTS", 5),
			PINLockDuration:    p.duration("WALLET_PIN_LOCK_DURATION", 30*time.Minute),
			MoneyRequestTTL:    p.duration("MONEY_REQUEST_TTL", 7*24*time.Hour),
			AccountHashSecret:  []byte(p.str("ACCOUNT_HASH_SECRET", "")),
		},
	}

	// Well-known insecure defaults let `go run` work out of the box outside
	// production; Validate rejects them in production.
	if cfg.Env != EnvProduction {
		if len(cfg.Auth.JWTSecret) == 0 {
			cfg.Auth.JWTSecret = []byte("dev-only-jwt-secret-change-me-0123456789")
		}
		if len(cfg.Auth.OTPSecret) == 0 {
			cfg.Auth.OTPSecret = []byte("dev-only-otp-secret-change-me-0123456789")
		}
		if len(cfg.Wallet.AccountHashSecret) == 0 {
			cfg.Wallet.AccountHashSecret = []byte("dev-only-account-hash-secret-0123456789")
		}
	}

	if err := errors.Join(p.err, cfg.Validate()); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	switch c.Env {
	case EnvDevelopment, EnvTest, EnvProduction:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be development, test or production, got %q", c.Env))
	}
	if len(c.Auth.JWTSecret) < minSecretBytes {
		errs = append(errs, fmt.Errorf("JWT_SECRET must be at least %d bytes", minSecretBytes))
	}
	if len(c.Auth.OTPSecret) < minSecretBytes {
		errs = append(errs, fmt.Errorf("OTP_SECRET must be at least %d bytes", minSecretBytes))
	}
	if len(c.Wallet.AccountHashSecret) < minSecretBytes {
		errs = append(errs, fmt.Errorf("ACCOUNT_HASH_SECRET must be at least %d bytes", minSecretBytes))
	}
	w := c.Wallet
	if w.MinPerTxnPence < 1 || w.MaxPerTxnPence < w.MinPerTxnPence || w.DailyOutgoingPence < w.MaxPerTxnPence || w.MaxBalancePence < w.MaxPerTxnPence {
		errs = append(errs, errors.New("wallet limits must satisfy 1 <= MIN_PER_TXN <= MAX_PER_TXN <= DAILY_OUTGOING and MAX_PER_TXN <= MAX_BALANCE"))
	}
	if w.PINMaxAttempts < 1 || w.MaxBankAccounts < 1 || w.MoneyRequestTTL <= 0 {
		errs = append(errs, errors.New("WALLET_PIN_MAX_ATTEMPTS, WALLET_MAX_BANK_ACCOUNTS and MONEY_REQUEST_TTL must be positive"))
	}
	if c.Env == EnvProduction {
		if strings.HasPrefix(string(c.Auth.JWTSecret), "dev-only") || strings.HasPrefix(string(c.Auth.OTPSecret), "dev-only") ||
			strings.HasPrefix(string(c.Wallet.AccountHashSecret), "dev-only") {
			errs = append(errs, errors.New("development secrets must not be used in production"))
		}
		if c.Auth.SMSProvider == "log" {
			errs = append(errs, errors.New("SMS_PROVIDER=log is not allowed in production"))
		}
	}
	if c.Auth.OTPLength < 4 || c.Auth.OTPLength > 8 {
		errs = append(errs, errors.New("OTP_LENGTH must be between 4 and 8"))
	}
	if c.Auth.OTPMaxAttempts < 1 {
		errs = append(errs, errors.New("OTP_MAX_ATTEMPTS must be positive"))
	}
	if c.Auth.AccessTokenTTL <= 0 || c.Auth.RefreshTokenTTL <= c.Auth.AccessTokenTTL {
		errs = append(errs, errors.New("token TTLs must be positive and REFRESH_TOKEN_TTL > ACCESS_TOKEN_TTL"))
	}
	if c.Username.BloomBackend != "memory" && c.Username.BloomBackend != "redis" {
		errs = append(errs, errors.New("USERNAME_BLOOM_BACKEND must be memory or redis"))
	}
	if c.Username.BloomFPRate <= 0 || c.Username.BloomFPRate >= 1 {
		errs = append(errs, errors.New("USERNAME_BLOOM_FP_RATE must be in (0,1)"))
	}
	return errors.Join(errs...)
}

func (c Config) IsDevelopment() bool { return c.Env == EnvDevelopment }

type parser struct {
	getenv func(string) string
	err    error
}

func (p *parser) str(key, def string) string {
	if v := strings.TrimSpace(p.getenv(key)); v != "" {
		return v
	}
	return def
}

func (p *parser) list(key string) []string {
	var out []string
	for _, part := range strings.Split(p.getenv(key), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (p *parser) int(key string, def int) int {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		p.err = errors.Join(p.err, fmt.Errorf("%s: invalid integer %q", key, v))
		return def
	}
	return i
}

func (p *parser) float(key string, def float64) float64 {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		p.err = errors.Join(p.err, fmt.Errorf("%s: invalid number %q", key, v))
		return def
	}
	return f
}

func (p *parser) location(key, def string) *time.Location {
	name := p.str(key, def)
	loc, err := time.LoadLocation(name)
	if err != nil {
		p.err = errors.Join(p.err, fmt.Errorf("%s: unknown time zone %q", key, name))
		return time.UTC
	}
	return loc
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.err = errors.Join(p.err, fmt.Errorf("%s: invalid duration %q", key, v))
		return def
	}
	return d
}
