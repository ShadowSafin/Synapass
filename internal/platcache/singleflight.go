package platcache

import (
	"errors"
	"sync"
	"time"
)

var errStoreDown = errors.New("platcache: store unavailable")

// flightGroup coalesces concurrent rebuilds of the same key (single-flight).
// Only the first caller runs fn; the rest wait briefly and share the result.
// A bounded wait keeps a slow DB from piling every gateway worker behind one
// key: waiters that time out fall back to stale data or a direct load.
type flightGroup struct {
	mu      sync.Mutex
	calls   map[string]*flightCall
	waits   int64
	coatime time.Duration
}

type flightCall struct {
	done   chan struct{}
	value  []byte
	stored time.Time
	err    error
}

func newFlightGroup(coalesceTimeout time.Duration) *flightGroup {
	if coalesceTimeout <= 0 {
		coalesceTimeout = 3 * time.Second
	}
	return &flightGroup{calls: map[string]*flightCall{}, coatime: coalesceTimeout}
}

// Do runs fn unless another goroutine is already rebuilding key, in which
// case it waits for that rebuild. shared reports coalescing.
func (g *flightGroup) Do(key string, fn func() ([]byte, time.Time, error)) ([]byte, time.Time, bool, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*flightCall{}
	}
	if c, ok := g.calls[key]; ok {
		g.waits++
		g.mu.Unlock()
		timer := time.NewTimer(g.coatime)
		defer timer.Stop()
		select {
		case <-c.done:
			return c.value, c.stored, true, c.err
		case <-timer.C:
			return nil, time.Time{}, true, errCoalesceTimeout
		}
	}
	c := &flightCall{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	value, stored, err := fn()
	c.value, c.stored, c.err = value, stored, err
	close(c.done)

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return value, stored, false, err
}

var errCoalesceTimeout = errors.New("platcache: coalesce wait timed out")
