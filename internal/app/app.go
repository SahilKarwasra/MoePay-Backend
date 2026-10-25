package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/sahilkarwasra/moepay/internal/auth"
	"github.com/sahilkarwasra/moepay/internal/bank"
	"github.com/sahilkarwasra/moepay/internal/chat"
	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/internal/dashboard"
	"github.com/sahilkarwasra/moepay/internal/history"
	"github.com/sahilkarwasra/moepay/internal/idempotency"
	"github.com/sahilkarwasra/moepay/internal/ledger"
	"github.com/sahilkarwasra/moepay/internal/moneyrequest"
	"github.com/sahilkarwasra/moepay/internal/onboarding"
	"github.com/sahilkarwasra/moepay/internal/payment"
	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
	"github.com/sahilkarwasra/moepay/internal/ratelimit"
	"github.com/sahilkarwasra/moepay/internal/realtime"
	"github.com/sahilkarwasra/moepay/internal/user"
	"github.com/sahilkarwasra/moepay/internal/username"
	"github.com/sahilkarwasra/moepay/internal/wallet"
)

type App struct {
	Config    config.Config
	Logger    *slog.Logger
	Pool      *pgxpool.Pool
	Redis     *redis.Client
	Engine    *gin.Engine
	Hub       *realtime.Hub
	Usernames *username.Checker
	Auth      *auth.Service
	authRepo  *auth.Repository
	requests  *moneyrequest.Service
}

// Options overrides dependencies, e.g. tests capture OTPs with a fake SMSSender.
type Options struct {
	SMS         auth.SMSSender
	BankGateway bank.Gateway
}

func New(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool, rdb *redis.Client, opts Options) (*App, error) {
	users := user.NewRepository()
	limiter := ratelimit.New(rdb, logger)
	hub := realtime.NewHub(rdb, cfg.WS.MaxConnsPerUser, logger)

	var backend username.Backend = username.MemoryBackend{}
	if cfg.Username.BloomBackend == "redis" {
		m, k := username.OptimalParams(cfg.Username.BloomExpectedItems, cfg.Username.BloomFPRate)
		backend = username.NewRedisBackend(rdb, fmt.Sprintf("bloom:usernames:%d:%d", m, k))
	}
	names := username.NewChecker(usernameSource{pool: pool, repo: users}, backend,
		cfg.Username.BloomExpectedItems, cfg.Username.BloomFPRate, logger)

	onboardingSvc := onboarding.NewService(pool, onboarding.NewRepository(), users, names, hub)

	sms := opts.SMS
	if sms == nil {
		var err error
		if sms, err = auth.NewSMSSender(cfg.Auth.SMSProvider, logger); err != nil {
			return nil, err
		}
	}
	authRepo := auth.NewRepository()
	authSvc := auth.NewService(auth.Deps{
		Config:     cfg.Auth,
		Pool:       pool,
		Repo:       authRepo,
		Users:      users,
		Onboarding: onboardingSvc,
		Tokens:     auth.NewTokenIssuer(cfg.Auth.JWTSecret, cfg.Auth.JWTIssuer, cfg.Auth.AccessTokenTTL),
		OTP:        auth.NewOTPCodec(cfg.Auth.OTPSecret, cfg.Auth.OTPLength),
		SMS:        sms,
		Limiter:    limiter,
		Revoked:    auth.NewRevocationCache(rdb, cfg.Auth.AccessTokenTTL),
		Events:     hub,
		Logger:     logger,
	})

	// E-wallet. The ledger is the only writer of balances; wallets open when
	// onboarding completes, in the same transaction.
	books := ledger.New(cfg.Wallet)
	walletSvc := wallet.NewService(pool, wallet.NewRepository(), users, authSvc, cfg.Wallet)
	onboardingSvc.OnComplete(walletSvc.OnOnboardingCompleted)
	bankRepo := bank.NewRepository()
	bankSvc := bank.NewService(pool, bankRepo, walletSvc, cfg.Wallet)
	gateway := opts.BankGateway
	if gateway == nil {
		gateway = bank.DummyGateway{}
	}
	historyReader := history.NewReader()
	paymentSvc := payment.NewService(payment.Deps{
		Pool: pool, Ledger: books, Wallets: walletSvc, Banks: bankRepo, Gateway: gateway, Users: users,
		History: historyReader, Events: hub, DefaultRegion: cfg.Auth.DefaultRegion, Logger: logger,
	})
	requestSvc := moneyrequest.NewService(pool, moneyrequest.NewRepository(), books, walletSvc, paymentSvc, historyReader, hub, cfg.Wallet)

	// Chat. Registering the service hooks payments and requests into conversations.
	chatSvc := chat.NewService(chat.Deps{
		Pool: pool, Repo: chat.NewRepository(), Wallets: walletSvc, Payments: paymentSvc, Requests: requestSvc,
		Events: hub, Config: cfg.Chat, Logger: logger,
	})
	chatHandler := chat.NewHandler(chatSvc, limiter)
	hub.Handle("typing", chatHandler.HandleTyping)

	if cfg.Env != config.EnvDevelopment {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()
	engine.HandleMethodNotAllowed = true
	if err := engine.SetTrustedProxies(cfg.HTTP.TrustedProxies); err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}
	engine.Use(
		httpx.RequestContext(logger),
		httpx.Recover(),
		httpx.AccessLog(),
		httpx.SecurityHeaders(),
		httpx.CORS(cfg.HTTP.CORSOrigins),
	)
	engine.NoRoute(func(c *gin.Context) { httpx.Fail(c, notFound) })
	engine.NoMethod(func(c *gin.Context) { httpx.Fail(c, methodNotAllowed) })

	a := &App{Config: cfg, Logger: logger, Pool: pool, Redis: rdb, Engine: engine, Hub: hub, Usernames: names,
		Auth: authSvc, authRepo: authRepo, requests: requestSvc}

	engine.GET("/healthz", func(c *gin.Context) { httpx.OK(c, http.StatusOK, "ok", nil) })
	engine.GET("/readyz", a.ready)

	authHandler := auth.NewHandler(authSvc)
	public := engine.Group("/api/v1")
	authed := engine.Group("/api/v1", authHandler.Middleware())

	authHandler.Register(public, authed)
	user.NewHandler(pool, users).Register(authed)
	onboarding.NewHandler(onboardingSvc, names, limiter).Register(authed)
	realtime.NewHandler(hub, authSvc, auth.BearerToken, rdb, cfg.WS, logger).Register(public, authed)

	wallet.NewHandler(walletSvc, limiter).Register(authed)
	bank.NewHandler(bankSvc).Register(authed)
	payment.NewHandler(paymentSvc, limiter).Register(authed)
	moneyrequest.NewHandler(requestSvc, limiter).Register(authed)
	history.NewHandler(pool, historyReader, walletSvc, cfg.Wallet.Location).Register(authed)
	chatHandler.Register(authed)
	dashboard.NewHandler(dashboard.Deps{
		Pool: pool, Users: users, Wallets: walletSvc, Banks: bankRepo, Ledger: books,
		History: historyReader, Payments: paymentSvc, Requests: requestSvc,
	}).Register(authed)

	return a, nil
}

func (a *App) ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	checks := gin.H{"postgres": "ok", "redis": "ok"}
	status := http.StatusOK
	if err := a.Pool.Ping(ctx); err != nil {
		checks["postgres"], status = "unavailable", http.StatusServiceUnavailable
	}
	if err := a.Redis.Ping(ctx).Err(); err != nil {
		// Redis degrades features (rate limits fail open) but the API still serves.
		checks["redis"] = "unavailable"
	}
	if status != http.StatusOK {
		c.JSON(status, httpx.Response{StatusCode: status, Success: false, Message: "not ready", Data: checks})
		return
	}
	httpx.OK(c, status, "ready", checks)
}

// RunBackground starts long-running jobs; they stop when ctx is cancelled.
func (a *App) RunBackground(ctx context.Context) {
	go a.Hub.Run(ctx)
	go a.Usernames.Run(ctx, a.Config.Username.BloomRebuildEvery)
	go a.purgeLoop(ctx)
	go a.expireRequestsLoop(ctx)
}

func (a *App) expireRequestsLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := a.requests.ExpireDue(ctx)
			if err != nil {
				a.Logger.Error("expire money requests failed", "err", err)
			} else if n > 0 {
				a.Logger.Info("expired money requests", "count", n)
			}
		}
	}
}

func (a *App) purgeLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			otps, sessions, err := a.authRepo.PurgeExpired(ctx, a.Pool, 30*24*time.Hour)
			if err != nil {
				a.Logger.Error("purge expired auth data failed", "err", err)
				continue
			}
			keys, err := idempotency.Purge(ctx, a.Pool)
			if err != nil {
				a.Logger.Error("purge idempotency keys failed", "err", err)
				continue
			}
			a.Logger.Info("purged expired data", "otp_challenges", otps, "sessions", sessions, "idempotency_keys", keys)
		}
	}
}

type usernameSource struct {
	pool *pgxpool.Pool
	repo *user.Repository
}

func (s usernameSource) UsernameExists(ctx context.Context, normalized string) (bool, error) {
	return s.repo.UsernameExists(ctx, s.pool, normalized)
}

func (s usernameSource) EachUsername(ctx context.Context, batch int, fn func(string) error) (int, error) {
	return s.repo.EachUsername(ctx, s.pool, batch, fn)
}
