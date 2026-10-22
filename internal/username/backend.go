package username

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrRebuildInProgress means another instance holds the rebuild lock.
var ErrRebuildInProgress = errors.New("bloom rebuild in progress elsewhere")

// Backend owns where the filter's bits live and how a rebuilt filter replaces the
// live one atomically.
type Backend interface {
	// Begin returns an empty store to populate and a publish func that atomically
	// makes it the live store.
	Begin(ctx context.Context, m uint64) (build BitStore, publish func(context.Context) (BitStore, error), err error)
	// Live returns the current shared store if one was published by any instance.
	Live(ctx context.Context) (BitStore, bool, error)
	Name() string
}

// MemoryBackend keeps the filter in process. Each instance builds its own copy, so
// with several instances a name claimed elsewhere is only seen after the next
// rebuild; the unique index still prevents duplicates.
type MemoryBackend struct{}

func (MemoryBackend) Name() string { return "memory" }

func (MemoryBackend) Begin(_ context.Context, m uint64) (BitStore, func(context.Context) (BitStore, error), error) {
	s := NewMemoryBits(m)
	return s, func(context.Context) (BitStore, error) { return s, nil }, nil
}

func (MemoryBackend) Live(context.Context) (BitStore, bool, error) { return nil, false, nil }

// RedisBackend shares one filter across all instances. Rebuilds are written to a
// temporary key and swapped in with RENAME under a lock, so readers never see a
// half-built filter.
type RedisBackend struct {
	rdb *redis.Client
	key string
}

func NewRedisBackend(rdb *redis.Client, key string) *RedisBackend {
	return &RedisBackend{rdb: rdb, key: key}
}

func (b *RedisBackend) Name() string { return "redis" }

func (b *RedisBackend) readyKey() string { return b.key + ":ready" }
func (b *RedisBackend) lockKey() string  { return b.key + ":lock" }

func (b *RedisBackend) Begin(ctx context.Context, _ uint64) (BitStore, func(context.Context) (BitStore, error), error) {
	token := randomToken()
	ok, err := b.rdb.SetNX(ctx, b.lockKey(), token, 10*time.Minute).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("acquire bloom lock: %w", err)
	}
	if !ok {
		return nil, nil, ErrRebuildInProgress
	}
	tmp := b.key + ":build:" + token
	publish := func(ctx context.Context) (BitStore, error) {
		defer b.release(ctx, token)
		pipe := b.rdb.TxPipeline()
		// An empty build never created tmp; make sure the live key is empty too.
		pipe.Del(ctx, b.key)
		pipe.Eval(ctx, `if redis.call('EXISTS', KEYS[1]) == 1 then return redis.call('RENAME', KEYS[1], KEYS[2]) end return 0`, []string{tmp, b.key})
		pipe.Set(ctx, b.readyKey(), time.Now().UTC().Format(time.RFC3339), 0)
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, fmt.Errorf("publish bloom: %w", err)
		}
		return NewRedisBits(b.rdb, b.key), nil
	}
	return NewRedisBits(b.rdb, tmp), publish, nil
}

var releaseScript = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`)

func (b *RedisBackend) release(ctx context.Context, token string) {
	_ = releaseScript.Run(ctx, b.rdb, []string{b.lockKey()}, token).Err()
}

func (b *RedisBackend) Live(ctx context.Context) (BitStore, bool, error) {
	n, err := b.rdb.Exists(ctx, b.readyKey()).Result()
	if err != nil {
		return nil, false, err
	}
	return NewRedisBits(b.rdb, b.key), n == 1, nil
}

func randomToken() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
