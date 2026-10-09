package providers

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// maxStreamLineBytes bounds a single stream line. Tool-call argument fragments
// can be large but not unbounded; a cap prevents a hostile or buggy upstream
// from exhausting memory through one endless line.
const maxStreamLineBytes = 4 << 20

// sseEvent is one parsed Server-Sent Events frame.
type sseEvent struct {
	// Event is the "event:" field, empty for the default message event.
	Event string
	// Data is the concatenation of all "data:" lines, joined with newlines per
	// the SSE specification.
	Data string
	// ID is the "id:" field, used to resume a stream.
	ID string
	// Retry is the server-suggested reconnection delay in milliseconds.
	Retry int
}

// done reports whether the frame is the OpenAI end-of-stream sentinel.
func (e sseEvent) done() bool {
	return strings.TrimSpace(e.Data) == "[DONE]"
}

// empty reports whether the frame carried no payload and should be skipped.
func (e sseEvent) empty() bool {
	return strings.TrimSpace(e.Data) == "" && e.Event == ""
}

// sseReader parses an SSE stream incrementally.
//
// It is a hand-written parser rather than a library because the only features
// Synapass needs are the "event", "data" and comment fields, and because the
// streaming path is hot enough that avoiding reflection-based decoding is worth
// the few dozen lines.
type sseReader struct {
	scanner *bufio.Scanner
	// pending holds a decoded frame when the caller asked for the next event
	// but the reader had already advanced past it.
	eof bool
}

// newSSEReader wraps a reader in an SSE parser.
func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxStreamLineBytes)
	// Split on newlines; Scanner strips \n and leaves a trailing \r to trim.
	sc.Split(bufio.ScanLines)
	return &sseReader{scanner: sc}
}

// Next returns the next event.
//
// It returns (event, true, nil) for a frame, (zero, false, nil) at clean EOF and
// (zero, false, err) on a read error. A clean EOF without a trailing blank line is
// common because many providers simply close the connection, so it is not an
// error.
func (r *sseReader) Next() (sseEvent, bool, error) {
	var (
		ev        sseEvent
		sawField  bool
		dataLines []string
	)

	for {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return sseEvent{}, false, err
			}
			// EOF: flush a frame that was not terminated by a blank line.
			if sawField {
				ev.Data = strings.Join(dataLines, "\n")
				return ev, true, nil
			}
			return sseEvent{}, false, nil
		}

		line := r.scanner.Text()
		// A carriage return is left in place by ScanLines when the stream uses
		// CRLF, which is legal in SSE.
		line = strings.TrimSuffix(line, "\r")

		if line == "" {
			// Blank line dispatches the buffered frame.
			if !sawField {
				continue
			}
			ev.Data = strings.Join(dataLines, "\n")
			return ev, true, nil
		}

		if strings.HasPrefix(line, ":") {
			// Comment. Providers commonly send ": ping" as a keepalive; it must
			// not reset the idle timer as if it were content.
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			// A field with no colon has an empty value.
			field, value = line, ""
		}
		// A single leading space after the colon is part of the framing.
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "event":
			ev.Event = value
			sawField = true
		case "data":
			dataLines = append(dataLines, value)
			sawField = true
		case "id":
			ev.ID = value
			sawField = true
		case "retry":
			ev.Retry = atoiSafe(value)
			sawField = true
		default:
			// Unknown fields are ignored per the specification.
		}
	}
}

// Close releases any buffered state. It does not close the underlying reader,
// which is owned by the HTTP response.
func (r *sseReader) Close() { r.scanner = nil }

// ndjsonReader reads newline-delimited JSON, the format Ollama uses for
// streaming chat responses.
type ndjsonReader struct {
	scanner *bufio.Scanner
}

// newNDJSONReader wraps a reader in an NDJSON parser.
func newNDJSONReader(r io.Reader) *ndjsonReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxStreamLineBytes)
	return &ndjsonReader{scanner: sc}
}

// Next returns the next non-empty line's bytes.
func (r *ndjsonReader) Next() ([]byte, bool, error) {
	for r.scanner.Scan() {
		line := bytes.TrimSpace(r.scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// The scanner reuses its buffer, so copy before returning.
		out := make([]byte, len(line))
		copy(out, line)
		return out, true, nil
	}
	if err := r.scanner.Err(); err != nil {
		return nil, false, err
	}
	return nil, false, nil
}

// atoiSafe parses a decimal integer, returning zero on any problem. Stream
// metadata must never abort a response.
func atoiSafe(s string) int {
	n := 0
	neg := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-' && i == 0:
			neg = true
		case c >= '0' && c <= '9':
			n = n*10 + int(c-'0')
		default:
			return 0
		}
	}
	if neg {
		return -n
	}
	return n
}

// streamTimers enforces the two liveness budgets of a streaming attempt:
//
//	first-token: the provider must produce its first event promptly after
//	  the upstream connection is established. A provider that accepts a
//	  request and then stays silent is stuck, not slow.
//	idle: the gap between successive events must stay bounded. Keepalive
//	  comments and empty frames do not count as activity.
//
// All three streaming adapters share this helper so a stalled stream fails
// fast with the same timeout semantics regardless of provider.
//
// The helper is safe for concurrent use: the parse loop records activity
// while a watchdog goroutine (see livenessWatcher) polls expiry, so a stall
// with zero bytes on the wire still aborts instead of burning the whole
// per-attempt budget.
type streamTimers struct {
	mu         sync.Mutex
	firstToken time.Duration
	idle       time.Duration
	start      time.Time
	last       time.Time
	gotFirst   bool
}

// newStreamTimers captures the budgets from the effective timeout policy.
func newStreamTimers(t domain.TimeoutPolicy) *streamTimers {
	now := time.Now()
	return &streamTimers{
		firstToken: t.FirstToken,
		idle:       t.StreamIdle,
		start:      now,
		last:       now,
	}
}

// onEvent records one meaningful event. It fails when the first event
// arrived after the first-token budget.
func (t *streamTimers) onEvent() *domain.Error {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.gotFirst {
		t.gotFirst = true
		if t.firstToken > 0 && now.Sub(t.start) > t.firstToken {
			return domain.NewError(domain.ErrCodeTimeout,
				"provider did not produce output in time")
		}
	}
	t.last = now
	return nil
}

// onEmpty checks the idle budget on an empty frame (keepalive comment or
// blank event). Content events go through onEvent instead.
func (t *streamTimers) onEmpty() *domain.Error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.idle > 0 && time.Since(t.last) > t.idle {
		return domain.NewError(domain.ErrCodeTimeout,
			"stream stalled waiting for provider data")
	}
	return nil
}

// expired reports the liveness failure if the stream has already exceeded a
// budget, without recording activity. The watchdog polls this while blocked
// reads cannot.
func (t *streamTimers) expired() *domain.Error {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.gotFirst {
		if t.firstToken > 0 && now.Sub(t.start) > t.firstToken {
			return domain.NewError(domain.ErrCodeTimeout,
				"provider did not produce output in time")
		}
		return nil
	}
	if t.idle > 0 && now.Sub(t.last) > t.idle {
		return domain.NewError(domain.ErrCodeTimeout,
			"stream stalled waiting for provider data")
	}
	return nil
}

// livenessWatcher aborts a stalled stream even when no bytes arrive.
//
// The parse loops block in network reads, so synchronous timer checks only
// run when data arrives. The watcher polls timers.expired on a ticker and
// cancels the attempt context when a budget is exceeded; cancelling aborts
// the upstream transport, which unblocks the read with an error that the
// loop then maps to the stored timeout (see Fired), not to a client cancel.
type livenessWatcher struct {
	timers *streamTimers
	cancel context.CancelFunc
	stop   chan struct{}
	done   chan struct{}

	mu       sync.Mutex
	firedErr *domain.Error
}

// startLivenessWatch launches the watchdog. The caller must defer Stop.
// A nil cancel or zero budgets leaves the watcher inert (Stop is still safe).
func startLivenessWatch(timers *streamTimers, cancel context.CancelFunc) *livenessWatcher {
	w := &livenessWatcher{
		timers: timers,
		cancel: cancel,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *livenessWatcher) run() {
	defer close(w.done)
	if w.timers == nil || w.cancel == nil {
		return
	}
	interval := watchInterval(w.timers.firstToken, w.timers.idle)
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			if err := w.timers.expired(); err != nil {
				w.mu.Lock()
				w.firedErr = err
				w.mu.Unlock()
				w.cancel()
				return
			}
		}
	}
}

// watchInterval polls at a quarter of the tightest budget, clamped so a
// 90s idle budget does not wake every 22s per stream while a 50ms test
// budget is still caught promptly.
func watchInterval(firstToken, idle time.Duration) time.Duration {
	best := firstToken
	if best <= 0 || (idle > 0 && idle < best) {
		best = idle
	}
	if best <= 0 {
		return 0
	}
	interval := best / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	return interval
}

// Stop terminates the watchdog and waits for it, so no abort can fire after
// the attempt returned. It is idempotent-safe when called once via defer.
func (w *livenessWatcher) Stop() {
	if w == nil {
		return
	}
	close(w.stop)
	<-w.done
}

// Fired returns the timeout that aborted the stream, or nil when the
// watchdog did not fire.
func (w *livenessWatcher) Fired() *domain.Error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.firedErr
}

// streamError wraps an error returned by the caller's StreamHandler so the
// adapter can distinguish "the consumer stopped" from "the provider failed" and
// avoid mislabelling a client disconnect as an upstream incident.
type streamError struct {
	err error
}

// Error implements the error interface.
func (e *streamError) Error() string { return e.err.Error() }

// Unwrap exposes the consumer's error.
func (e *streamError) Unwrap() error { return e.err }

// newStreamError marks an error as originating from the consumer.
func newStreamError(err error) error { return &streamError{err: err} }

// isStreamError reports whether err came from the consumer's handler.
func isStreamError(err error) bool {
	_, ok := err.(*streamError)
	return ok
}

// IsConsumerError reports whether err originated from the stream consumer
// (typically a client disconnect surfacing as a downstream write failure)
// rather than from the provider. Adapters wrap handler errors with
// newStreamError; this predicate lets other packages classify them without
// depending on the unexported type.
func IsConsumerError(err error) bool {
	return isStreamError(err)
}
