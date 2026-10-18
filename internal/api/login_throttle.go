package api

import (
	"sync"
	"time"
)

const (
	// loginAttemptLimit is the number of login/setup attempts one client may
	// make inside loginAttemptWindow. It is deliberately generous: the account
	// lockout is the precise control, and this limiter exists to bound the
	// Argon2id work an attacker can force (each attempt is a deliberate CPU
	// cost) rather than to police a legitimate operator's typos.
	loginAttemptLimit = 30
	// loginAttemptWindow is the fixed window for the attempt counter.
	loginAttemptWindow = time.Minute
	// loginThrottleMaxEntries bounds memory. Once the table is full, expired
	// entries are pruned; a full table of live entries means a distributed
	// attack, which this in-process limiter cannot solve anyway.
	loginThrottleMaxEntries = 10000
)

// loginThrottle is a fixed-window counter keyed by client address.
//
// The dashboard proxy means every console request arrives from the same
// loopback/container address in a native or compose deployment, so this is
// effectively a global ceiling there — which is the honest description: the
// gateway cannot see the browser's address behind its own proxy. The
// per-account lockout remains the primary defence, and this is the backstop
// that keeps a stream of attempts from turning into a CPU sink.
type loginThrottle struct {
	mu      sync.Mutex
	entries map[string]loginThrottleEntry
	now     func() time.Time
}

type loginThrottleEntry struct {
	count int
	// reset is when the current window ends.
	reset time.Time
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{entries: map[string]loginThrottleEntry{}, now: time.Now}
}

// allow records an attempt and reports whether it may proceed. When it may not,
// the returned duration is the time until the window resets.
func (t *loginThrottle) allow(key string) (bool, time.Duration) {
	if t == nil {
		return true, 0
	}
	if key == "" {
		key = "unknown"
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if len(t.entries) >= loginThrottleMaxEntries {
		t.pruneLocked(now)
	}

	entry, ok := t.entries[key]
	if !ok || now.After(entry.reset) {
		t.entries[key] = loginThrottleEntry{count: 1, reset: now.Add(loginAttemptWindow)}
		return true, 0
	}
	if entry.count >= loginAttemptLimit {
		return false, entry.reset.Sub(now)
	}
	entry.count++
	t.entries[key] = entry
	return true, 0
}

// reset clears a key's counter. A successful login does not need to call it —
// successes are rare relative to the limit — but clearing on success means an
// operator who mistyped a password a few times starts fresh after logging in.
func (t *loginThrottle) reset(key string) {
	if t == nil {
		return
	}
	if key == "" {
		key = "unknown"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

func (t *loginThrottle) pruneLocked(now time.Time) {
	for key, entry := range t.entries {
		if now.After(entry.reset) {
			delete(t.entries, key)
		}
	}
}
