package configstore

import (
	"context"
	"sort"
	"sync"
)

// MemStore is the in-memory implementation for tests and no-database dev. To
// keep its behaviour identical to SQLStore, values are held in memory as
// CIPHERTEXT (via Crypter.Seal) rather than plaintext: that exercises the
// Crypter round-trip on the same code path and guarantees the disabled state
// returns ErrConfigDisabled here exactly as it would from the database.
type MemStore struct {
	c  *Crypter
	mu sync.RWMutex
	m  map[string][]byte // key -> nonce||ciphertext
}

// NewMemStore constructs an in-memory store. A disabled Crypter makes every
// method return ErrConfigDisabled.
func NewMemStore(c *Crypter) *MemStore {
	return &MemStore{c: c, m: map[string][]byte{}}
}

var _ Store = (*MemStore)(nil)

func (s *MemStore) Get(_ context.Context, key string) (string, bool, error) {
	if !s.c.Enabled() {
		return "", false, ErrConfigDisabled
	}
	s.mu.RLock()
	sealed, ok := s.m[key]
	s.mu.RUnlock()
	if !ok {
		return "", false, nil
	}
	pt, err := s.c.Open(sealed)
	if err != nil {
		return "", false, err
	}
	return string(pt), true, nil
}

func (s *MemStore) Set(_ context.Context, key, plaintext string) error {
	if !s.c.Enabled() {
		return ErrConfigDisabled
	}
	sealed, err := s.c.Seal([]byte(plaintext))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.m[key] = sealed
	s.mu.Unlock()
	return nil
}

func (s *MemStore) Delete(_ context.Context, key string) error {
	if !s.c.Enabled() {
		return ErrConfigDisabled
	}
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
	return nil
}

func (s *MemStore) Keys(_ context.Context) ([]string, error) {
	if !s.c.Enabled() {
		return nil, ErrConfigDisabled
	}
	s.mu.RLock()
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	s.mu.RUnlock()
	sort.Strings(out)
	return out, nil
}
