package username

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// RedisBits stores the bit array in a Redis string (SETBIT/GETBIT) so all API
// instances share one filter.
type RedisBits struct {
	rdb *redis.Client
	key string
}

func NewRedisBits(rdb *redis.Client, key string) *RedisBits {
	return &RedisBits{rdb: rdb, key: key}
}

func (s *RedisBits) SetBits(ctx context.Context, positions []uint64) error {
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, pos := range positions {
			p.SetBit(ctx, s.key, int64(pos), 1)
		}
		return nil
	})
	return err
}

func (s *RedisBits) TestBits(ctx context.Context, positions []uint64) (bool, error) {
	cmds, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, pos := range positions {
			p.GetBit(ctx, s.key, int64(pos))
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, c := range cmds {
		if c.(*redis.IntCmd).Val() == 0 {
			return false, nil
		}
	}
	return true, nil
}
