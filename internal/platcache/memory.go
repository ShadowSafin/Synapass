package platcache

import (
	"sync"
	"time"
)

// memEntry is one L1 slot.
type memEntry struct {
	value     []byte
	expiresAt time.Time
	storedAt  time.Time
}

// Memory is a bounded in-process short-lived cache (L1).
// It is deliberately tiny: ultra-fast, very short TTL, safe read-heavy
// data only. Eviction is oldest-first past capacity.
type Memory struct {
	mu      sync.Mutex
	entries map[string]memEntry
	max     int
	hits    int64
	misses  int64
	evicts  int64
}

// NewMemory builds an L1 cache holding up to maxEntries keys.
func NewMemory(maxEntries int) *Memory {
	if maxEntries <= 0 {
		maxEntries = 2000
	}
	return &Memory{entries: make(map[string]memEntry, maxEntries), max: maxEntries}
}

// Get returns a copy of the value when present and unexpired.
func (m *Memory) Get(key string) ([]byte, bool) {
	if m == nil || key == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		m.misses++
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		delete(m.entries, key)
		m.misses++
		return nil, false
	}
	m.hits++
	return append([]byte{}, e.value...), true
}

// Set stores a copy with a TTL.
func (m *Memory) Set(key string, value []byte, ttl time.Duration) {
	if m == nil || key == "" || len(value) == 0 {
		return
	}
	if ttl <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) >= m.max {
		m.evictOldestLocked()
	}
	m.entries[key] = memEntry{
		value:     append([]byte{}, value...),
		expiresAt: time.Now().Add(ttl),
		storedAt:  time.Now(),
	}
}

// Delete removes a key.
func (m *Memory) Delete(key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}

// DeletePrefix removes every key with the prefix; returns the count.
func (m *Memory) DeletePrefix(prefix string) int {
	if m == nil || prefix == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(m.entries, k)
			n++
		}
	}
	return n
}

// Flush empties L1.
func (m *Memory) Flush() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.entries)
	m.entries = make(map[string]memEntry)
	return n
}

// Len returns the live entry count (expired entries excluded).
func (m *Memory) Len() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	n := 0
	for _, e := range m.entries {
		if now.Before(e.expiresAt) {
			n++
		}
	}
	return n
}

func (m *Memory) evictOldestLocked() {
	oldest := ""
	var oldestAt time.Time
	first := true
	for k, e := range m.entries {
		if first || e.storedAt.Before(oldestAt) {
			oldest, oldestAt, first = k, e.storedAt, false
		}
	}
	if oldest != "" {
		delete(m.entries, oldest)
		m.evicts++
	}
}
