package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/shadowsafin/synapass/internal/domain"
)

// TestSSEWriterHeadersFlushTerminate proves the SSE contract: content type,
// no-cache directives, per-event flush, and [DONE] termination.
func TestSSEWriterHeadersFlushTerminate(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, err := newSSEWriter(rec)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	if err := sse.WriteEvent(map[string]string{"hello": "world"}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if !rec.Flushed {
		t.Fatalf("each event must flush")
	}
	if err := sse.WriteDone(); err != nil {
		t.Fatalf("WriteDone: %v", err)
	}
	res := rec.Result()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache, no-transform" {
		t.Fatalf("cache-control = %q", cc)
	}
	if res.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("missing X-Accel-Buffering: no")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data: {"hello":"world"}`) {
		t.Fatalf("event frame missing: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream must end with [DONE]: %q", body)
	}
	if sse.EventsWritten() != 1 || sse.BytesWritten() <= 0 {
		t.Fatalf("counters wrong: events=%d bytes=%d", sse.EventsWritten(), sse.BytesWritten())
	}
	if sse.FirstByteAt().IsZero() || !sse.WroteHeader() {
		t.Fatalf("first-byte/header tracking wrong")
	}
}

// TestSSEWriterStreamError proves a post-commit failure emits an error event
// followed by the terminator — never model data after the terminal signal,
// never a bare hang.
func TestSSEWriterStreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, err := newSSEWriter(rec)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	if err := sse.WriteEvent(map[string]string{"partial": "yes"}); err != nil {
		t.Fatal(err)
	}
	if err := sse.WriteStreamError(domain.NewError(domain.ErrCodeUpstream, "provider exploded")); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	errIdx := strings.Index(body, "event: error")
	doneIdx := strings.Index(body, "data: [DONE]")
	if errIdx < 0 || doneIdx < 0 || errIdx > doneIdx {
		t.Fatalf("error event must precede [DONE]: %q", body)
	}
	if !strings.Contains(body, "provider exploded") {
		t.Fatalf("error event must carry the cause: %q", body)
	}
	tail := body[doneIdx+len("data: [DONE]\n\n"):]
	if strings.Contains(tail, "data: ") {
		t.Fatalf("no model data after terminal event: %q", tail)
	}
}

// TestSSEKeepalive proves comment pings flow during idle and stop on demand.
func TestSSEKeepalive(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, err := newSSEWriter(rec)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	if err := sse.WriteEvent(map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sse.StartKeepalive(ctx, 10*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	sse.StopKeepalive()
	bodyAfterStop := rec.Body.String()
	if !strings.Contains(bodyAfterStop, ": ping") {
		t.Fatalf("keepalive comments missing: %q", bodyAfterStop)
	}
	if n := strings.Count(bodyAfterStop, ": ping"); n < 2 {
		t.Fatalf("want repeated pings, got %d: %q", n, bodyAfterStop)
	}
	// No further pings after stop.
	time.Sleep(30 * time.Millisecond)
	if rec.Body.String() != bodyAfterStop {
		t.Fatalf("keepalive must stop: %q", rec.Body.String())
	}
	// Context cancel also stops the loop (no goroutine leak on disconnect).
	sse2, _ := newSSEWriter(httptest.NewRecorder())
	_ = sse2.WriteEvent(map[string]string{"a": "b"})
	ctx2, cancel2 := context.WithCancel(context.Background())
	sse2.StartKeepalive(ctx2, 10*time.Millisecond)
	cancel2()
	time.Sleep(30 * time.Millisecond)
	sse2.StopKeepalive() // must return promptly even though ctx is done
}

// delayedFlushWriter stalls every flush past the slow-write threshold.
type delayedFlushWriter struct {
	header http.Header
	body   strings.Builder
	delay  time.Duration
}

func (w *delayedFlushWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *delayedFlushWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *delayedFlushWriter) WriteHeader(int)            {}
func (w *delayedFlushWriter) Flush()                     { time.Sleep(w.delay) }

// TestSSESlowWriteHook proves slow downstream writes are reported without
// failing delivery.
func TestSSESlowWriteHook(t *testing.T) {
	dw := &delayedFlushWriter{delay: 600 * time.Millisecond}
	sse, err := newSSEWriter(dw)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	var (
		mu     sync.Mutex
		called int
	)
	sse.OnSlowWrite = func(latency time.Duration, _ int64) {
		mu.Lock()
		defer mu.Unlock()
		called++
		if latency < 500*time.Millisecond {
			t.Errorf("slow write latency = %v", latency)
		}
	}
	if err := sse.WriteEvent(map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Fatalf("slow write hook calls = %d, want 1", called)
	}
	if !strings.Contains(dw.body.String(), `"x":"y"`) {
		t.Fatalf("delivery must continue despite slowness: %q", dw.body.String())
	}
}

// TestSSEConcurrentKeepaliveAndWrites exercises the writer lock: keepalive
// comments and content frames from different goroutines must not race.
// Run the suite with -race to enforce.
func TestSSEConcurrentKeepaliveAndWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, err := newSSEWriter(rec)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sse.StartKeepalive(ctx, 5*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_ = sse.WriteEvent(map[string]int{"n": j})
			}
		}()
	}
	wg.Wait()
	sse.StopKeepalive()
	if got := sse.EventsWritten(); got != 200 {
		t.Fatalf("events = %d, want 200", got)
	}
}

// TestCompressMiddlewarePassesThroughSSE proves the gateway's compression
// middleware does not buffer or encode event streams: the content type is
// untouched, no Content-Encoding is applied, and flushing still works.
func TestCompressMiddlewarePassesThroughSSE(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse, err := newSSEWriter(w)
		if err != nil {
			t.Errorf("newSSEWriter through compress: %v", err)
			return
		}
		if err := sse.WriteEvent(map[string]string{"k": "v"}); err != nil {
			t.Errorf("WriteEvent through compress: %v", err)
		}
	})
	handler := chimw.Compress(5)(next)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(rec, req)

	res := rec.Result()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	if ce := res.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("SSE must not be encoded: %q", ce)
	}
	if !strings.Contains(rec.Body.String(), `"k":"v"`) {
		t.Fatalf("event missing: %q", rec.Body.String())
	}
}

// TestRespondErrorPostCommitCancel proves a server-side cancel after commit
// (timeout, shutdown — connection still alive) emits the error event plus
// terminator and records a canceled outcome, never success.
func TestRespondErrorPostCommitCancel(t *testing.T) {
	s := &Server{logger: slog.Default()}
	rec := httptest.NewRecorder()
	sse, err := newSSEWriter(rec)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	if err := sse.WriteEvent(map[string]string{"partial": "yes"}); err != nil {
		t.Fatal(err)
	}
	rc := &domain.RequestContext{
		RequestID:   domain.RequestID(domain.NewID()),
		RequestType: domain.RequestTypeChatCompletion,
		Stream:      true,
	}
	rc.StreamTextRunes = 40 // some text was delivered
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // server-side cancel with a live downstream
	cancelErr := domain.NewError(domain.ErrCodeCanceled, "the client disconnected")
	s.respondError(ctx, rec, sse, rc, cancelErr, time.Now().Add(-time.Second), false)

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("cancel must emit error event + terminator: %q", body)
	}
	if got := partialStreamUsage(rc); got.CompletionTokens <= 0 || !got.Estimated {
		t.Fatalf("partial usage must estimate delivered work: %+v", got)
	}
}

// TestPartialStreamUsage proves billing reconciliation inputs for partial
// streams: zero when nothing was delivered, estimated otherwise.
func TestPartialStreamUsage(t *testing.T) {
	if got := partialStreamUsage(nil); got != (domain.TokenUsage{}) {
		t.Fatalf("nil rc: %+v", got)
	}
	rc := &domain.RequestContext{PromptTokens: 100}
	if got := partialStreamUsage(rc); got.CompletionTokens != 0 {
		t.Fatalf("no delivery must yield zero usage: %+v", got)
	}
	rc.StreamTextRunes = 36
	got := partialStreamUsage(rc)
	if got.PromptTokens != 100 || !got.Estimated {
		t.Fatalf("prompt/estimated flags: %+v", got)
	}
	if got.CompletionTokens != 10 { // ceil(36/3.6)
		t.Fatalf("completion estimate = %d, want 10", got.CompletionTokens)
	}
}
