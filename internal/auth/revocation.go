package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RevocationCache marks revoked sessions in Redis for the lifetime of an access token,
// so the auth middleware can reject a signed-out access token without a database
// round trip on every request.
type RevocationCache struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewRevocationCache(rdb *redis.Client, accessTTL time.Duration) *RevocationCache {
	// Keep the marker slightly longer than the longest-lived access token (+ leeway).
	return &RevocationCache{rdb: rdb, ttl: accessTTL + time.Minute}
}

func revokedKey(sid uuid.UUID) string { return "sess:revoked:" + sid.String() }

func (c *RevocationCache) MarkRevoked(ctx context.Context, ids ...uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := c.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, id := range ids {
			p.Set(ctx, revokedKey(id), 1, c.ttl)
		}
		return nil
	})
	return err
}

func (c *RevocationCache) IsRevoked(ctx context.Context, sid uuid.UUID) (bool, error) {
	n, err := c.rdb.Exists(ctx, revokedKey(sid)).Result()
	return n == 1, err
}
