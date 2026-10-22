package username

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"math"
	"sync"
)

// BitStore holds a Bloom filter's bit array. Implementations must be safe for
// concurrent use.
type BitStore interface {
	SetBits(ctx context.Context, positions []uint64) error
	TestBits(ctx context.Context, positions []uint64) (bool, error)
}

// Bloom is a standard Bloom filter using Kirsch–Mitzenmacher double hashing over a
// deterministic 128-bit FNV-1a hash, so every process maps a name to the same bits
// (required when the bit array lives in Redis).
type Bloom struct {
	m, k  uint64
	store BitStore
}

// OptimalParams returns the bit count m and hash count k for n items at false-positive rate p.
func OptimalParams(n uint, p float64) (m, k uint64) {
	if n == 0 {
		n = 1
	}
	mf := math.Ceil(-float64(n) * math.Log(p) / (math.Ln2 * math.Ln2))
	kf := math.Round(mf / float64(n) * math.Ln2)
	return uint64(math.Max(mf, 64)), uint64(math.Max(kf, 1))
}

func NewBloom(m, k uint64, store BitStore) *Bloom {
	return &Bloom{m: m, k: k, store: store}
}

func (b *Bloom) positions(item string) []uint64 {
	h := fnv.New128a()
	_, _ = h.Write([]byte(item))
	sum := h.Sum(nil)
	h1 := binary.BigEndian.Uint64(sum[:8])
	h2 := binary.BigEndian.Uint64(sum[8:]) | 1 // odd step visits distinct positions
	out := make([]uint64, b.k)
	for i := uint64(0); i < b.k; i++ {
		out[i] = (h1 + i*h2) % b.m
	}
	return out
}

func (b *Bloom) Add(ctx context.Context, item string) error {
	return b.store.SetBits(ctx, b.positions(item))
}

// MightContain is false only if item was definitely never added.
func (b *Bloom) MightContain(ctx context.Context, item string) (bool, error) {
	return b.store.TestBits(ctx, b.positions(item))
}

// MemoryBits is an in-process bit array.
type MemoryBits struct {
	mu    sync.RWMutex
	words []uint64
}

func NewMemoryBits(m uint64) *MemoryBits {
	return &MemoryBits{words: make([]uint64, (m+63)/64)}
}

func (s *MemoryBits) SetBits(_ context.Context, positions []uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range positions {
		s.words[p/64] |= 1 << (p % 64)
	}
	return nil
}

func (s *MemoryBits) TestBits(_ context.Context, positions []uint64) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range positions {
		if s.words[p/64]&(1<<(p%64)) == 0 {
			return false, nil
		}
	}
	return true, nil
}
