package username

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Source is the authoritative username store (PostgreSQL).
type Source interface {
	UsernameExists(ctx context.Context, normalized string) (bool, error)
	EachUsername(ctx context.Context, batch int, fn func(string) error) (int, error)
}

type Availability struct {
	Username  string `json:"username"`
	Available bool   `json:"available"`
	// Reason is one of available, taken, reserved, invalid.
	Reason string `json:"reason"`
}

// Checker answers "is this username available?".
//
// A Bloom filter has no false negatives, so "not in filter" means no existing user
// had that name when the filter was built — the answer is returned without a query.
// "Maybe in filter" is always confirmed against PostgreSQL, so a false positive never
// rejects a free name. Staleness (names released since the build, or claimed on
// another instance with the memory backend) is bounded by periodic rebuilds, and the
// unique index on users.username_normalized is the final guard at claim time.
type Checker struct {
	src     Source
	backend Backend
	logger  *slog.Logger
	m, k    uint64

	mu         sync.RWMutex
	bloom      *Bloom // nil until the first build is published
	rebuilding bool
	pending    []string
}

func NewChecker(src Source, backend Backend, expectedItems uint, fpRate float64, logger *slog.Logger) *Checker {
	m, k := OptimalParams(expectedItems, fpRate)
	return &Checker{src: src, backend: backend, logger: logger, m: m, k: k}
}

// Check validates and checks availability of raw.
func (c *Checker) Check(ctx context.Context, raw string) (Availability, error) {
	display, normalized, err := Normalize(raw)
	if err != nil {
		return Availability{Username: raw, Available: false, Reason: "invalid"}, nil
	}
	if IsReserved(normalized) {
		return Availability{Username: display, Available: false, Reason: "reserved"}, nil
	}

	if b := c.current(); b != nil {
		maybe, err := b.MightContain(ctx, normalized)
		if err != nil {
			c.logger.WarnContext(ctx, "bloom lookup failed, falling back to database", "err", err)
		} else if !maybe {
			return Availability{Username: display, Available: true, Reason: "available"}, nil
		}
	}

	exists, err := c.src.UsernameExists(ctx, normalized)
	if err != nil {
		return Availability{}, err
	}
	if exists {
		return Availability{Username: display, Available: false, Reason: "taken"}, nil
	}
	return Availability{Username: display, Available: true, Reason: "available"}, nil
}

// Added records a newly claimed name. Call after the claiming transaction commits.
func (c *Checker) Added(ctx context.Context, normalized string) {
	c.mu.Lock()
	b := c.bloom
	if c.rebuilding {
		c.pending = append(c.pending, normalized)
	}
	c.mu.Unlock()
	if b != nil {
		if err := b.Add(ctx, normalized); err != nil {
			c.logger.WarnContext(ctx, "bloom add failed", "err", err)
		}
	}
}

// Ready reports whether a filter has been built; before that Check uses PostgreSQL only.
func (c *Checker) Ready() bool { return c.current() != nil }

func (c *Checker) current() *Bloom {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bloom
}

// Rebuild loads every username from the source into a fresh filter and swaps it in.
// Names claimed during the rebuild are replayed into the new filter before the swap.
func (c *Checker) Rebuild(ctx context.Context) error {
	start := time.Now()
	build, publish, err := c.backend.Begin(ctx, c.m)
	if errors.Is(err, ErrRebuildInProgress) {
		return c.attachLive(ctx)
	}
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.rebuilding, c.pending = true, nil
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.rebuilding, c.pending = false, nil
		c.mu.Unlock()
	}()

	fresh := NewBloom(c.m, c.k, build)
	n, err := c.src.EachUsername(ctx, 5000, func(name string) error { return fresh.Add(ctx, name) })
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range c.pending {
		if err := fresh.Add(ctx, name); err != nil {
			return err
		}
	}
	live, err := publish(ctx)
	if err != nil {
		return err
	}
	c.bloom = NewBloom(c.m, c.k, live)
	c.logger.InfoContext(ctx, "username bloom filter rebuilt",
		"backend", c.backend.Name(), "items", n, "bits", c.m, "hashes", c.k,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

// attachLive uses a filter another instance has published (Redis backend only).
func (c *Checker) attachLive(ctx context.Context) error {
	live, ready, err := c.backend.Live(ctx)
	if err != nil || !ready {
		return err
	}
	c.mu.Lock()
	c.bloom = NewBloom(c.m, c.k, live)
	c.mu.Unlock()
	return nil
}

// Run builds the filter and rebuilds it every interval until ctx is cancelled.
// Until the first build succeeds, Check goes straight to PostgreSQL.
func (c *Checker) Run(ctx context.Context, every time.Duration) {
	if err := c.Rebuild(ctx); err != nil && ctx.Err() == nil {
		c.logger.Error("initial username bloom build failed; using database only", "err", err)
	}
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Rebuild(ctx); err != nil && ctx.Err() == nil {
				c.logger.Error("username bloom rebuild failed; keeping previous filter", "err", err)
			}
		}
	}
}
