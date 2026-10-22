package redisx

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/sahilkarwasra/moepay/internal/config"
)

func Connect(ctx context.Context, cfg config.RedisConfig) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis at %s: %w", cfg.Addr, err)
	}
	return client, nil
}
