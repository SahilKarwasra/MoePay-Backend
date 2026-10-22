package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/migrations"
)

// DBTX is satisfied by *pgxpool.Pool and pgx.Tx so repositories work in and out of transactions.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func Connect(ctx context.Context, cfg config.PostgresConfig) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	// Unnamed statements only: required behind PgBouncer-style poolers (e.g. Neon's
	// -pooler endpoint) and harmless on a direct connection.
	pcfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// WithTx runs fn inside a transaction, committing on success and rolling back on error or panic.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
			}
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// Postgres error codes used for mapping constraint violations.
const (
	CodeUniqueViolation     = "23505"
	CodeCheckViolation      = "23514"
	CodeForeignKeyViolation = "23503"
)

// ConstraintError reports the violated constraint name if err is a Postgres error with one of codes.
func ConstraintError(err error, codes ...string) (string, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "", false
	}
	for _, c := range codes {
		if pgErr.Code == c {
			return pgErr.ConstraintName, true
		}
	}
	return "", false
}

// Migrate applies schema migrations. command is one of up, down, status, version, redo, reset.
func Migrate(ctx context.Context, pool *pgxpool.Pool, command string, logger *slog.Logger) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	return runGoose(ctx, db, command, logger)
}

func runGoose(ctx context.Context, db *sql.DB, command string, logger *slog.Logger) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(gooseLogger{logger})
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	switch command {
	case "up":
		return goose.UpContext(ctx, db, ".")
	case "down":
		return goose.DownContext(ctx, db, ".")
	case "redo":
		return goose.RedoContext(ctx, db, ".")
	case "status":
		return goose.StatusContext(ctx, db, ".")
	case "version":
		return goose.VersionContext(ctx, db, ".")
	case "reset":
		return goose.ResetContext(ctx, db, ".")
	default:
		return fmt.Errorf("unknown migrate command %q", command)
	}
}

type gooseLogger struct{ l *slog.Logger }

func (g gooseLogger) Fatalf(format string, v ...any) { g.l.Error(fmt.Sprintf(format, v...)) }
func (g gooseLogger) Printf(format string, v ...any) { g.l.Info(fmt.Sprintf(format, v...)) }
