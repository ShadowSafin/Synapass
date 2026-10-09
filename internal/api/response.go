package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// writeJSON renders a response body.
//
// Encoding happens into a buffer before any header is written. That ordering is
// what allows an encoding failure to still produce a valid error response instead
// of a truncated body with a 200 status, which is the classic way a JSON handler
// fails silently.
func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		// Encoding our own response is a programming error, so the fallback is a
		// hand-written valid error body rather than another encoder call.
		http.Error(w, `{"error":{"message":"failed to encode the response","type":"server_error","code":"internal_error"}}`,
			http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(status)
	if _, err := w.Write(payload); err != nil {
		// A write failure here means the client is gone; there is nothing useful
		// left to do and logging at debug level avoids noise on every disconnect.
		slog.Default().Debug("failed to write response body", "error", err)
	}
}

// writeError renders a normalized error as an OpenAI-compatible envelope.
func writeError(w http.ResponseWriter, err error, meta *domain.ResponseMetadata) {
	normalized := domain.AsError(err)
	if normalized == nil {
		writeJSON(w, http.StatusInternalServerError, domain.ErrorResponse{
			Error: domain.ErrorBody{
				Message: "an internal error occurred",
				Type:    "server_error",
				Code:    string(domain.ErrCodeInternal),
			},
		})
		return
	}

	status := normalized.HTTPStatus()

	// The metadata block is attached to errors as well as successes, so a client
	// that logs the failure also captures which provider and policy were involved.
	body := struct {
		Error      domain.ErrorBody         `json:"error"`
		Synapass *domain.ResponseMetadata `json:"synapass,omitempty"`
	}{
		Error: domain.ErrorBody{
			Message: normalized.Message,
			Type:    normalized.ErrorType(),
			Param:   normalized.ErrorParam(),
			Code:    string(normalized.Code),
		},
		Synapass: meta,
	}

	// Some downstream proxies rewrite status codes, so the upstream status is
	// echoed in a header for callers that need the original.
	if normalized.Status > 0 && normalized.Status != status {
		w.Header().Set("X-Synapass-Upstream-Status", strconv.Itoa(normalized.Status))
	}
	if normalized.Provider != "" {
		w.Header().Set("X-Synapass-Provider", normalized.Provider)
	}
	if normalized.Retryable || normalized.FallbackEligible {
		w.Header().Set("X-Synapass-Retryable", strconv.FormatBool(normalized.Retryable))
	}

	writeJSON(w, status, body)
}

// slowWriteThreshold marks a downstream write as backpressured. A flush that
// takes this long means the client (or an intermediary) is reading slowly;
// the event is counted, never retried, and delivery continues.
const slowWriteThreshold = 500 * time.Millisecond

// keepaliveInterval is the SSE comment cadence during idle streams. Comments
// are protocol-level no-ops: they reset proxy idle timers without looking
// like model output.
const keepaliveInterval = 15 * time.Second

// sseWriter streams server-sent events.
//
// Writing SSE is deceptively fiddly: the response must be flushed after every
// event or a proxy will buffer the whole stream, and a client disconnect surfaces
// as a write error that must not be treated as a gateway fault.
//
// All writes are serialized under mu because keepalives are emitted from a
// background goroutine while content frames are written on the request
// goroutine; concurrent Write calls on an http.ResponseWriter are a data race.
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	// wroteHeader tracks whether the status line has been sent, because the first
	// write commits it.
	wroteHeader bool
	// bytes counts the payload written, used for the response size metric.
	bytes int64
	// firstByteAt is when the first byte reached the client. Zero until then.
	firstByteAt time.Time
	// events counts content frames (WriteEvent calls), excluding keepalives.
	// Atomic because WriteEvent increments it outside the write lock.
	events atomic.Int64
	// OnSlowWrite, when set, is called (while holding no locks) for every
	// write+flush slower than slowWriteThreshold.
	OnSlowWrite func(latency time.Duration, bytes int64)

	keepMu   sync.Mutex
	keepStop chan struct{}
	keepDone chan struct{}
}

// newSSEWriter prepares a response for streaming.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without a flusher the stream would be buffered until completion, which
		// defeats the entire purpose. Failing loudly here is better than silently
		// degrading to a buffered response.
		return nil, domain.NewError(domain.ErrCodeInternal,
			"streaming is not supported by this server configuration")
	}

	// These headers are what stop intermediaries from buffering or transforming
	// the stream.
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	// Nginx needs this to disable response buffering for the location.
	header.Set("X-Accel-Buffering", "no")

	return &sseWriter{w: w, flusher: flusher}, nil
}

// WriteEvent writes one SSE data frame and flushes it.
func (s *sseWriter) WriteEvent(payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return domain.NewError(domain.ErrCodeInternal, "failed to encode a stream event").Wrap(err)
	}
	// The event count is incremented before the write so a failed write still
	// leaves an honest "attempted" count; BytesWritten stays write-accurate.
	s.events.Add(1)
	return s.WriteRaw("data: " + string(encoded) + "\n\n")
}

// WriteRaw writes a pre-encoded frame and flushes it.
func (s *sseWriter) WriteRaw(frame string) error {
	start := time.Now()
	s.mu.Lock()
	if !s.wroteHeader {
		// 200 is correct even for a request that will fail mid-stream: the
		// provider call has already started and there is no meaningful status to
		// send. Failures before the first chunk are handled before this point.
		s.w.WriteHeader(http.StatusOK)
		s.wroteHeader = true
	}

	n, err := s.w.Write([]byte(frame))
	s.bytes += int64(n)
	if err == nil {
		if s.firstByteAt.IsZero() {
			s.firstByteAt = time.Now()
		}
		s.flusher.Flush()
	}
	onSlow := s.OnSlowWrite
	s.mu.Unlock()

	// Slow-write accounting happens outside the lock so a slow client never
	// holds the writer mutex while the metric is recorded.
	if err == nil && onSlow != nil && time.Since(start) >= slowWriteThreshold {
		onSlow(time.Since(start), int64(n))
	}
	return err
}

// WriteDone writes the OpenAI end-of-stream sentinel.
func (s *sseWriter) WriteDone() error {
	return s.WriteRaw("data: [DONE]\n\n")
}

// WriteStreamError reports a failure that occurred after the stream started.
//
// There is no status code left to change, so the failure is delivered as an error
// event followed by the terminator. A client that understands SSE sees a clean end
// of stream plus an error event; one that does not still sees a terminated stream
// rather than a hang.
func (s *sseWriter) WriteStreamError(err error) error {
	normalized := domain.AsError(err)
	body := domain.NewErrorResponse(normalized)
	encoded, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		encoded = []byte(`{"error":{"message":"stream failed","type":"server_error"}}`)
	}
	// The explicit "error" event name lets clients subscribe to failures rather
	// than having to inspect every data frame.
	if err := s.WriteRaw("event: error\ndata: " + string(encoded) + "\n\n"); err != nil {
		return err
	}
	return s.WriteDone()
}

// BytesWritten returns the payload size.
func (s *sseWriter) BytesWritten() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// EventsWritten returns the number of content frames attempted.
func (s *sseWriter) EventsWritten() int64 { return s.events.Load() }

// FirstByteAt returns when the first byte reached the client, or zero when
// nothing has been written yet.
func (s *sseWriter) FirstByteAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstByteAt
}

// WroteHeader reports whether the status line has been committed.
func (s *sseWriter) WroteHeader() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wroteHeader
}

// StartKeepalive emits an SSE comment every interval until the context ends
// or StopKeepalive is called. Comments reset intermediary idle timers without
// appearing as model output. It must only be called after the first content
// frame: starting it earlier would commit a 200 before the provider answers
// and turn pre-provider failures into in-stream errors.
func (s *sseWriter) StartKeepalive(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = keepaliveInterval
	}
	s.keepMu.Lock()
	if s.keepStop != nil {
		s.keepMu.Unlock()
		return // already running
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.keepStop, s.keepDone = stop, done
	s.keepMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				// A comment frame; write errors mean the client is gone, so
				// the loop exits rather than spinning on a dead connection.
				if err := s.WriteRaw(": ping\n\n"); err != nil {
					return
				}
			}
		}
	}()
}

// StopKeepalive terminates the keepalive loop and waits for it, so a
// stopped stream does not leak its goroutine. A comment already in flight
// may still land around a terminal event; clients ignore comments, so this
// is harmless.
func (s *sseWriter) StopKeepalive() {
	s.keepMu.Lock()
	stop, done := s.keepStop, s.keepDone
	s.keepStop, s.keepDone = nil, nil
	s.keepMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// parseIntParam reads an integer query parameter with a default.
func parseIntParam(r *http.Request, name string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

// parseBoolParam reads a boolean query parameter with a default.
func parseBoolParam(r *http.Request, name string, fallback bool) bool {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

// decodeJSONBody decodes a request body with a size limit.
func decodeJSONBody(r *http.Request, limit int64, target any) error {
	if r.Body == nil {
		return domain.NewError(domain.ErrCodeInvalidRequest, "a request body is required")
	}
	if limit > 0 {
		r.Body = http.MaxBytesReader(nil, r.Body, limit)
	}

	decoder := json.NewDecoder(r.Body)
	// Unknown fields are rejected: a typo in a parameter name would otherwise be
	// silently ignored, which produces a confusing "why is my setting ignored?".
	decoder.DisallowUnknownFields()
	decoder.UseNumber()

	if err := decoder.Decode(target); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return domain.Errorf(domain.ErrCodeInvalidRequest,
				"the request body exceeds the %d byte limit", maxBytesErr.Limit)
		}
		// A syntax error is the most common cause and deserves an actionable
		// message rather than the raw decoder text.
		return domain.Errorf(domain.ErrCodeInvalidRequest,
			"failed to parse the request body as JSON: %v", err)
	}
	return nil
}

// headerDebug opts a client into full routing internals on the inference
// surface. The default response carries stable attribution only (who answered,
// what was served, at what cost); policy names, strategies, task labels and
// route reasons stay behind this header so normal app clients never depend on
// operational details that change between deploys. The admin surface and the
// dashboard always see the full block.
const headerDebug = "X-Synapass-Debug"

// debugRequested reports whether the client asked for routing internals.
func debugRequested(r *http.Request) bool {
	if r == nil {
		return false
	}
	raw := r.Header.Get(headerDebug)
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	return err == nil && value
}

// publicMeta builds the client-facing metadata block: stable attribution by
// default, full routing internals in debug mode.
func publicMeta(r *http.Request, rc *domain.RequestContext, extra func(*domain.ResponseMetadata)) *domain.ResponseMetadata {
	return publicMetaFrom(rc, extra, debugRequested(r))
}

// publicMetaFrom is publicMeta with the decision made by the caller, for the
// handlers that compute it once and thread it through writers and helpers.
func publicMetaFrom(rc *domain.RequestContext, extra func(*domain.ResponseMetadata), debug bool) *domain.ResponseMetadata {
	meta := metaFromContext(rc, extra)
	if meta == nil || debug {
		return meta
	}
	return &domain.ResponseMetadata{
		RequestID:        meta.RequestID,
		Provider:         meta.Provider,
		RequestedModel:   meta.RequestedModel,
		RoutedModel:      meta.RoutedModel,
		FallbackUsed:     meta.FallbackUsed,
		CacheHit:         meta.CacheHit,
		CacheKind:        meta.CacheKind,
		LatencyMS:        meta.LatencyMS,
		EstimatedCostUSD: meta.EstimatedCostUSD,
		// The tool run summary is part of the stable contract, not an
		// operational detail: a client that asked for a tool run needs to know
		// whether the gateway ran it, how many steps it took and why it stopped.
		// Structured-output conformance travels with it for the same reason.
		ToolRun:    meta.ToolRun,
		Structured: meta.Structured,
		// Completion bounding is part of the same contract: a caller that
		// receives a shortened answer must be able to tell that it was capped
		// rather than finished, without re-running the request to find out.
		Completion: meta.Completion,
	}
}

// metaFromContext builds the full response metadata block, including routing
// internals. It is the right choice for the admin surface and for internal
// bookkeeping; the public inference surface uses publicMeta instead.
func metaFromContext(rc *domain.RequestContext, extra func(*domain.ResponseMetadata)) *domain.ResponseMetadata {
	if rc == nil {
		return nil
	}
	meta := &domain.ResponseMetadata{
		RequestID: rc.RequestID.String(),
		TraceID:   rc.TraceID,
	}
	if rc.Tenant != nil {
		// Tenant identity is not exposed in the response body by default; only the
		// request id and trace id are, because a response may be logged verbatim by
		// a client that should not learn internal identifiers.
	}
	if rc.Task.Task != "" {
		meta.Task = string(rc.Task.Task)
	}
	if rc.ShapePlan.Explain() != "shaping: none" {
		meta.Shaping = rc.ShapePlan.Explain()
	}
	if rc.Resolution != nil {
		// Provider attribution is deliberately NOT set here. The decision records
		// where the request was sent first, not who answered it: after a failover
		// the two differ, and reporting the chosen target as the serving one would
		// mislabel exactly the requests an operator most needs to trace. Callers
		// that know the outcome set Provider themselves.
		meta.PolicyID = rc.Resolution.PolicyID
		meta.PolicyName = rc.Resolution.PolicyName
		meta.RequestedModel = rc.RequestedModel
		meta.RoutedModel = rc.Resolution.Chosen.Model
		meta.Strategy = string(rc.Resolution.Strategy)
		meta.EstimatedCostUSD = rc.Resolution.EstimatedCost.USD
		meta.RouteReason = rc.Resolution.Reason
		meta.Degraded = rc.Resolution.Degraded
		if rc.Resolution.CacheKind != "" {
			meta.CacheKind = rc.Resolution.CacheKind
		}
	}
	if extra != nil {
		extra(meta)
	}
	return meta
}

// describeError renders an error for logs without exposing credentials.
func describeError(err error) string {
	if err == nil {
		return ""
	}
	normalized := domain.AsError(err)
	if normalized == nil {
		return ""
	}
	if normalized.Provider != "" {
		return fmt.Sprintf("%s: %s (provider=%s)", normalized.Code, normalized.Message, normalized.Provider)
	}
	return fmt.Sprintf("%s: %s", normalized.Code, normalized.Message)
}
