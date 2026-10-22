package ratelimit

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type Limiter struct {
	rdb    *redis.Client
	prefix string
	logger *slog.Logger
}

func New(rdb *redis.Client, logger *slog.Logger) *Limiter {
	return &Limiter{rdb: rdb, prefix: "rl:", logger: logger}
}

// Rule is a named limit: at most Limit events per Window.
type Rule struct {
	Name   string
	Limit  int
	Window time.Duration
}

type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

var incrScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return {n, redis.call('PTTL', KEYS[1])}
`)

// Allow records one event for key under rule and reports whether it is within the limit.
func (l *Limiter) Allow(ctx context.Context, rule Rule, key string) Result {
	if rule.Limit <= 0 {
		return Result{Allowed: true}
	}
	k := l.prefix + rule.Name + ":" + key
	res, err := incrScript.Run(ctx, l.rdb, []string{k}, rule.Window.Milliseconds()).Int64Slice()
	if err != nil || len(res) != 2 {
		l.logger.WarnContext(ctx, "rate limiter unavailable, failing open", "rule", rule.Name, "err", err)
		return Result{Allowed: true, Remaining: rule.Limit}
	}
	count, pttl := int(res[0]), time.Duration(res[1])*time.Millisecond
	if pttl < 0 {
		pttl = rule.Window
	}
	if count > rule.Limit {
		return Result{Allowed: false, RetryAfter: pttl}
	}
	return Result{Allowed: true, Remaining: rule.Limit - count}
}
