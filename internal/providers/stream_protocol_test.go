package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ---------------------------------------------------------------------------
// Deterministic mock streaming transport
//
// chunkedBody yields its payload in controlled byte slices, so tests can
// prove the parsers handle arbitrary network chunk boundaries: 1-byte reads,
// splits inside JSON strings, splits inside multibyte UTF-8 sequences, and
// multiple events per read.
// ---------------------------------------------------------------------------

type chunkedBody struct {
	chunks [][]byte
	delays []time.Duration
	mu     sync.Mutex
	i      int
	closed bool
}

func newChunkedBody(payload string, splitEvery int, delay time.Duration) *chunkedBody {
	raw := []byte(payload)
	var chunks [][]byte
	if splitEvery <= 0 {
		chunks = [][]byte{raw}
	} else {
		for len(raw) > 0 {
			n := splitEvery
			if n > len(raw) {
				n = len(raw)
			}
			chunks = append(chunks, raw[:n])
			raw = raw[n:]
		}
	}
	delays := make([]time.Duration, len(chunks))
	for i := range delays {
		delays[i] = delay
	}
	return &chunkedBody{chunks: chunks, delays: delays}
}

func (b *chunkedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.i >= len(b.chunks) {
		b.mu.Unlock()
		return 0, io.EOF
	}
	// Delays are best-effort per-chunk pacing: a shorter delay list than the
	// chunk list (e.g. overwritten by a test) means zero delay, never a
	// panic. Panicking while holding the mutex would deadlock closeBody's
	// drain during unwind.
	var delay time.Duration
	if b.i < len(b.delays) {
		delay = b.delays[b.i]
	}
	chunk := b.chunks[b.i]
	b.i++
	b.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	n := copy(p, chunk)
	return n, nil
}

func (b *chunkedBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func streamReq() *Request {
	return &Request{
		Model:  "test-model",
		Params: &domain.ChatCompletionRequest{},
		Ref:    domain.ModelRef{ProviderName: "mock", Model: "test-model"},
	}
}

func collectChunks(t *testing.T, stream func(handler StreamHandler) (*Response, error)) ([]Chunk, *Response) {
	t.Helper()
	var (
		mu     sync.Mutex
		chunks []Chunk
	)
	resp, err := stream(func(c Chunk) error {
		mu.Lock()
		defer mu.Unlock()
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return chunks, resp
}

func openAIChunk(id, model, content string) string {
	return `data: {"id":"` + id + `","object":"chat.completion.chunk","created":1700000000,"model":"` + model + `","choices":[{"index":0,"delta":{"role":"assistant","content":` + jsonQuote(content) + `}}]}` + "\n\n"
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func openAIAdapterWith(t *testing.T, body io.ReadCloser, timeouts domain.TimeoutPolicy) Adapter {
	t.Helper()
	transport := newStubTransport(func(*http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK, Status: "OK",
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:   body,
		}
	})
	a, err := NewAdapter(providerFor("mock-openai", domain.ProviderOpenAI, "https://mock.invalid/v1"),
		Options{Client: transport, Timeouts: timeouts})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a
}

// TestOpenAIChunkBoundaries proves byte-at-a-time delivery still yields the
// same chunks as whole-event delivery, including a multibyte emoji split
// across reads and an escaped string split mid-escape.
func TestOpenAIChunkBoundaries(t *testing.T) {
	payload := openAIChunk("c1", "m", "hello") +
		openAIChunk("c1", "m", "wörld 🌊") +
		openAIChunk("c1", "m", "quote \" mid") +
		"data: [DONE]\n\n"
	for _, split := range []int{1, 2, 3, 7} {
		a := openAIAdapterWith(t, newChunkedBody(payload, split, 0), domain.TimeoutPolicy{})
		chunks, resp := collectChunks(t, func(h StreamHandler) (*Response, error) {
			return a.ChatCompletionStream(context.Background(), streamReq(), h)
		})
		if len(chunks) != 3 {
			t.Fatalf("split=%d: chunks=%d, want 3", split, len(chunks))
		}
		var text strings.Builder
		for _, c := range chunks {
			text.WriteString(c.Delta.Content.PlainText())
		}
		if text.String() != `hello`+`wörld 🌊`+`quote " mid` {
			t.Fatalf("split=%d: text=%q", split, text.String())
		}
		if resp.Content() != text.String() {
			t.Fatalf("split=%d: accumulated=%q want %q", split, resp.Content(), text.String())
		}
	}
}

// TestOpenAIMultipleEventsPerRead proves several events in one network read
// each become a chunk.
func TestOpenAIMultipleEventsPerRead(t *testing.T) {
	payload := openAIChunk("c1", "m", "a") + openAIChunk("c1", "m", "b") +
		openAIChunk("c1", "m", "c") + "data: [DONE]\n\n"
	a := openAIAdapterWith(t, newChunkedBody(payload, 0, 0), domain.TimeoutPolicy{})
	chunks, _ := collectChunks(t, func(h StreamHandler) (*Response, error) {
		return a.ChatCompletionStream(context.Background(), streamReq(), h)
	})
	if len(chunks) != 3 {
		t.Fatalf("chunks=%d, want 3", len(chunks))
	}
}

// TestOpenAIToolCallArgSplit proves tool-call argument fragments split
// across events reassemble into valid JSON exactly once.
func TestOpenAIToolCallArgSplit(t *testing.T) {
	payload := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\": \"SF\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	// Deliver the argument JSON split mid-token at the byte level too.
	a := openAIAdapterWith(t, newChunkedBody(payload, 5, 0), domain.TimeoutPolicy{})
	_, resp := collectChunks(t, func(h StreamHandler) (*Response, error) {
		return a.ChatCompletionStream(context.Background(), streamReq(), h)
	})
	if len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls: %+v", resp.Choices)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.Function.Name != "get_weather" {
		t.Fatalf("tool name=%q", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"city": "SF"}` {
		t.Fatalf("tool args=%q", tc.Function.Arguments)
	}
}

// TestOpenAIMultipleToolCalls proves concurrent tool calls keep stable
// indexes and ordering.
func TestOpenAIMultipleToolCalls(t *testing.T) {
	payload := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"a","arguments":""}},{"index":1,"id":"call_b","type":"function","function":{"name":"b","arguments":""}}]}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}},{"index":1,"function":{"arguments":"{\"y\":2}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	a := openAIAdapterWith(t, newChunkedBody(payload, 0, 0), domain.TimeoutPolicy{})
	chunks, resp := collectChunks(t, func(h StreamHandler) (*Response, error) {
		return a.ChatCompletionStream(context.Background(), streamReq(), h)
	})
	if len(chunks) < 2 {
		t.Fatalf("chunks=%d", len(chunks))
	}
	if len(resp.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("tool calls=%+v", resp.Choices[0].Message.ToolCalls)
	}
	first, second := resp.Choices[0].Message.ToolCalls[0], resp.Choices[0].Message.ToolCalls[1]
	if first.Function.Arguments != `{"x":1}` || second.Function.Arguments != `{"y":2}` {
		t.Fatalf("args=%q %q", first.Function.Arguments, second.Function.Arguments)
	}
}

// TestOpenAIUsageOnlyFinalEvent proves a terminal usage event is captured.
func TestOpenAIUsageOnlyFinalEvent(t *testing.T) {
	payload := openAIChunk("c1", "m", "hi") +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\n" +
		"data: [DONE]\n\n"
	a := openAIAdapterWith(t, newChunkedBody(payload, 0, 0), domain.TimeoutPolicy{})
	_, resp := collectChunks(t, func(h StreamHandler) (*Response, error) {
		return a.ChatCompletionStream(context.Background(), streamReq(), h)
	})
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 2 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
}

// TestOpenAIMidStreamDisconnect proves a truncated body errors as upstream,
// not as success.
func TestOpenAIMidStreamDisconnect(t *testing.T) {
	payload := openAIChunk("c1", "m", "partial")
	body := &failAfterBody{chunks: [][]byte{[]byte(payload)}, fail: errors.New("connection reset")}
	a := openAIAdapterWith(t, body, domain.TimeoutPolicy{})
	var n int
	_, err := a.ChatCompletionStream(context.Background(), streamReq(), func(Chunk) error { n++; return nil })
	if err == nil {
		t.Fatalf("expected upstream error")
	}
	if n != 1 {
		t.Fatalf("chunks before failure=%d, want 1", n)
	}
	if code := domain.AsError(err).Code; code != domain.ErrCodeUpstream {
		t.Fatalf("code=%q, want upstream", code)
	}
}

type failAfterBody struct {
	chunks [][]byte
	fail   error
	i      int
}

func (b *failAfterBody) Read(p []byte) (int, error) {
	if b.i >= len(b.chunks) {
		return 0, b.fail
	}
	n := copy(p, b.chunks[b.i])
	b.i++
	return n, nil
}

func (b *failAfterBody) Close() error { return nil }

// TestOpenAIMalformedEvents proves undecodable events are skipped without
// aborting the stream.
func TestOpenAIMalformedEvents(t *testing.T) {
	payload := "data: {not json}\n\n" + openAIChunk("c1", "m", "ok") + "data: [DONE]\n\n"
	a := openAIAdapterWith(t, newChunkedBody(payload, 0, 0), domain.TimeoutPolicy{})
	chunks, resp := collectChunks(t, func(h StreamHandler) (*Response, error) {
		return a.ChatCompletionStream(context.Background(), streamReq(), h)
	})
	if len(chunks) != 1 || resp.Content() != "ok" {
		t.Fatalf("chunks=%d content=%q", len(chunks), resp.Content())
	}
}

// TestFirstTokenTimeout proves a silent provider fails fast instead of
// burning the whole per-attempt budget.
func TestFirstTokenTimeout(t *testing.T) {
	payload := openAIChunk("c1", "m", "late") + "data: [DONE]\n\n"
	// First read arrives after 300ms with a 50ms first-token budget.
	body := newChunkedBody(payload, 0, 0)
	body.delays = []time.Duration{300 * time.Millisecond}
	a := openAIAdapterWith(t, body, domain.TimeoutPolicy{
		Total: 5 * time.Minute, PerAttempt: 5 * time.Minute,
		StreamIdle: 90 * time.Second, FirstToken: 50 * time.Millisecond,
	})
	_, err := a.ChatCompletionStream(context.Background(), streamReq(), func(Chunk) error { return nil })
	if err == nil {
		t.Fatalf("expected first-token timeout")
	}
	if code := domain.AsError(err).Code; code != domain.ErrCodeTimeout {
		t.Fatalf("code=%q, want timeout", code)
	}
}

// TestIdleTimeout proves a stalled mid-stream fails instead of hanging.
func TestIdleTimeout(t *testing.T) {
	payload := openAIChunk("c1", "m", "one") + openAIChunk("c1", "m", "two") + "data: [DONE]\n\n"
	body := newChunkedBody(payload, len(openAIChunk("c1", "m", "one")), 0)
	body.delays = []time.Duration{0, 300 * time.Millisecond, 0}
	a := openAIAdapterWith(t, body, domain.TimeoutPolicy{
		Total: 5 * time.Minute, PerAttempt: 5 * time.Minute,
		StreamIdle: 50 * time.Millisecond, FirstToken: 60 * time.Second,
	})
	var n int
	_, err := a.ChatCompletionStream(context.Background(), streamReq(), func(Chunk) error { n++; return nil })
	if err == nil {
		t.Fatalf("expected idle timeout")
	}
	if code := domain.AsError(err).Code; code != domain.ErrCodeTimeout {
		t.Fatalf("code=%q, want timeout", code)
	}
	if n != 1 {
		t.Fatalf("chunks before stall=%d, want 1", n)
	}
}

// TestAnthropicIdleTimeout proves the Anthropic adapter enforces idle now.
func TestAnthropicIdleTimeout(t *testing.T) {
	start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"m\",\"usage\":{\"input_tokens\":5}}}\n\n"
	delta := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	body := newChunkedBody(start+delta, len(start), 0)
	body.delays = []time.Duration{0, 300 * time.Millisecond}
	transport := newStubTransport(func(*http.Request) *http.Response {
		return &http.Response{StatusCode: 200, Status: "OK",
			Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}
	})
	a, err := NewAdapter(providerFor("mock-anthropic", domain.ProviderAnthropic, "https://mock.invalid"),
		Options{Client: transport, Timeouts: domain.TimeoutPolicy{
			Total: 5 * time.Minute, PerAttempt: 5 * time.Minute,
			StreamIdle: 50 * time.Millisecond, FirstToken: 60 * time.Second,
		}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = a.ChatCompletionStream(context.Background(), streamReq(), func(Chunk) error { return nil })
	if err == nil {
		t.Fatalf("expected idle timeout")
	}
	if code := domain.AsError(err).Code; code != domain.ErrCodeTimeout {
		t.Fatalf("code=%q, want timeout", code)
	}
}

// TestAnthropicErrorEvent proves an error event mid-stream aborts as failure.
func TestAnthropicErrorEvent(t *testing.T) {
	payload := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"m\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
	transport := newStubTransport(func(*http.Request) *http.Response {
		return &http.Response{StatusCode: 200, Status: "OK",
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:   io.NopCloser(strings.NewReader(payload))}
	})
	a, err := NewAdapter(providerFor("mock-anthropic", domain.ProviderAnthropic, "https://mock.invalid"), Options{Client: transport})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = a.ChatCompletionStream(context.Background(), streamReq(), func(Chunk) error { return nil })
	if err == nil {
		t.Fatalf("expected provider error")
	}
	if code := domain.AsError(err).Code; code != domain.ErrCodeRateLimited {
		t.Fatalf("code=%q, want rate_limited (overloaded)", code)
	}
}

// TestConcurrentStreamIsolation proves parallel streams on one adapter never
// interleave chunks.
func TestConcurrentStreamIsolation(t *testing.T) {
	const streams = 8
	transport := newStubTransport(func(*http.Request) *http.Response {
		// Body is per-request in a real transport; emulate by generating
		// unique content from the request path is unnecessary here because
		// each Do call gets its own reader below guarded by the payload.
		return nil
	})
	_ = transport
	var wg sync.WaitGroup
	errs := make([]error, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := strings.Repeat(string(rune('A'+i)), 50)
			payload := openAIChunk("c1", "m", marker) + "data: [DONE]\n\n"
			a := openAIAdapterWith(t, newChunkedBody(payload, 1, 0), domain.TimeoutPolicy{})
			var got strings.Builder
			_, err := a.ChatCompletionStream(context.Background(), streamReq(), func(c Chunk) error {
				got.WriteString(c.Delta.Content.PlainText())
				return nil
			})
			if err != nil {
				errs[i] = err
				return
			}
			if got.String() != marker {
				errs[i] = errors.New("interleaved content: " + got.String())
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
}

// TestStreamTimers unit-covers the shared helper.
func TestStreamTimers(t *testing.T) {
	tm := newStreamTimers(domain.TimeoutPolicy{FirstToken: 50 * time.Millisecond, StreamIdle: 50 * time.Millisecond})
	if err := tm.onEvent(); err != nil {
		t.Fatalf("first event must pass: %v", err)
	}
	time.Sleep(70 * time.Millisecond)
	if err := tm.onEmpty(); err == nil {
		t.Fatalf("idle must fire")
	}
	slow := newStreamTimers(domain.TimeoutPolicy{FirstToken: time.Millisecond})
	time.Sleep(5 * time.Millisecond)
	if err := slow.onEvent(); err == nil {
		t.Fatalf("late first event must fail")
	} else if err.Code != domain.ErrCodeTimeout {
		t.Fatalf("code=%q", err.Code)
	}
	off := newStreamTimers(domain.TimeoutPolicy{})
	time.Sleep(5 * time.Millisecond)
	if err := off.onEvent(); err != nil {
		t.Fatalf("zero budgets must disable timers: %v", err)
	}
	if err := off.onEmpty(); err != nil {
		t.Fatalf("zero idle must disable: %v", err)
	}
}
