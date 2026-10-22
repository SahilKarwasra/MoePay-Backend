package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/internal/platform/logging"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
)

func main() {
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if err := run(command); err != nil {
		slog.Error("migrate failed", "err", err)
		os.Exit(1)
	}
}

func run(command string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(string(cfg.Env), cfg.LogLevel)
	if command == "reset" || (command == "down" && cfg.Env == config.EnvProduction) {
		if os.Getenv("MIGRATE_ALLOW_DESTRUCTIVE") != "yes" {
			return fmt.Errorf("%q is destructive; set MIGRATE_ALLOW_DESTRUCTIVE=yes to proceed", command)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()
	return postgres.Migrate(ctx, pool, command, logger)
}
