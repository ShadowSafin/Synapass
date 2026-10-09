package platcache

import (
	"context"
	"sync"
	"time"
)

// Store is the L2 backend contract. *storage.Redis satisfies it via the
// redisAdapter; tests use the in-process MapStore. Implementations must be
// safe for concurrent use.
type Store interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	DeletePrefix(ctx context.Context, prefix string) (int, error)
	// SetNX sets the key only when absent; used for distributed locks.
	// It reports whether the key was created.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
}

// MapStore is an in-process Store for tests and single-binary deployments.
type MapStore struct {
	mu   sync.Mutex
	data map[string]mapEntry
}

type mapEntry struct {
	value     []byte
	expiresAt time.Time
}

// NewMapStore builds an empty store.
func NewMapStore() *MapStore { return &MapStore{data: map[string]mapEntry{}} }

func (s *MapStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[key]
	if !ok {
		return nil, false, nil
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		delete(s.data, key)
		return nil, false, nil
	}
	return append([]byte{}, e.value...), true, nil
}

func (s *MapStore) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]mapEntry{}
	}
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	s.data[key] = mapEntry{value: append([]byte{}, value...), expiresAt: exp}
	return nil
}

func (s *MapStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *MapStore) DeletePrefix(_ context.Context, prefix string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(s.data, k)
			n++
		}
	}
	return n, nil
}

func (s *MapStore) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.data[key]; ok {
		if e.expiresAt.IsZero() || time.Now().Before(e.expiresAt) {
			return false, nil
		}
	}
	if s.data == nil {
		s.data = map[string]mapEntry{}
	}
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	s.data[key] = mapEntry{value: append([]byte{}, value...), expiresAt: exp}
	return true, nil
}

// FailingStore always errors; used to prove Redis-down fallback.
type FailingStore struct{ Err error }

func (s *FailingStore) Get(_ context.Context, _ string) ([]byte, bool, error) {
	if s.Err != nil {
		return nil, false, s.Err
	}
	return nil, false, errStoreDown
}

func (s *FailingStore) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	if s.Err != nil {
		return s.Err
	}
	return errStoreDown
}

func (s *FailingStore) Delete(_ context.Context, _ string) error {
	if s.Err != nil {
		return s.Err
	}
	return errStoreDown
}

func (s *FailingStore) DeletePrefix(_ context.Context, _ string) (int, error) {
	if s.Err != nil {
		return 0, s.Err
	}
	return 0, errStoreDown
}

func (s *FailingStore) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	if s.Err != nil {
		return false, s.Err
	}
	return false, errStoreDown
}
