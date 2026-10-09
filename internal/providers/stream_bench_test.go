package providers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ---------------------------------------------------------------------------
// Deterministic mock streaming adapter
//
// scriptAdapter emits a scripted chunk sequence with controllable first-token
// delay, inter-chunk delay, failures, and completion — no network, no live
// provider account. Benchmarks below measure gateway-side overhead against
// these controlled upstreams.
// ---------------------------------------------------------------------------

type scriptAdapter struct {
	name       string
	firstDelay time.Duration
	interDelay time.Duration
	textChunks []string
	// failAfter aborts with an upstream error after this many chunks (-1: complete).
	failAfter int
	// hang blocks until ctx is done (cancellation tests).
	hang bool

	firstAt time.Time
}

func (a *scriptAdapter) Name() string              { return a.name }
func (a *scriptAdapter) Kind() domain.ProviderKind { return domain.ProviderOpenAI }
func (a *scriptAdapter) Capabilities() domain.CapabilitySet {
	return domain.NewCapabilitySet(domain.CapChat, domain.CapStreaming)
}
func (a *scriptAdapter) Enabled() bool { return true }
func (a *scriptAdapter) HealthCheck(context.Context) domain.ProviderHealth {
	return domain.ProviderHealth{ProviderName: a.name, State: domain.HealthHealthy, CheckedAt: domain.Now(), Source: "script"}
}
func (a *scriptAdapter) ChatCompletion(ctx context.Context, req *Request) (*Response, error) {
	var b strings.Builder
	for _, c := range a.textChunks {
		b.WriteString(c)
	}
	msg := &domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(b.String())}
	finish := domain.FinishStop
	return &Response{ID: "bench", Model: req.Model, Created: 1,
		Choices: []domain.Choice{{Index: 0, Message: msg, FinishReason: &finish}}}, nil
}

func (a *scriptAdapter) ChatCompletionStream(ctx context.Context, req *Request, handler StreamHandler) (*Response, error) {
	if a.hang {
		<-ctx.Done()
		return nil, NormalizeTransportError(a.name, req.Model, 1, ctx.Err())
	}
	if a.firstDelay > 0 {
		select {
		case <-ctx.Done():
			return nil, NormalizeTransportError(a.name, req.Model, 1, ctx.Err())
		case <-time.After(a.firstDelay):
		}
	}
	acc := newStreamAccumulator()
	for i, text := range a.textChunks {
		// failAfter delivers that many chunks and then fails (0 = complete).
		if a.failAfter > 0 && i+1 > a.failAfter {
			return nil, domain.NewError(domain.ErrCodeUpstream, "scripted mid-stream failure").
				WithProvider(a.name, req.Model, 1)
		}
		if i > 0 && a.interDelay > 0 {
			select {
			case <-ctx.Done():
				return nil, NormalizeTransportError(a.name, req.Model, 1, ctx.Err())
			case <-time.After(a.interDelay):
			}
		}
		chunk := Chunk{ID: "bench", Model: req.Model, Index: 0,
			Delta: domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(text)}}
		acc.ObserveChunk(chunk)
		if handler != nil {
			if err := handler(chunk); err != nil {
				return nil, newStreamError(err)
			}
		}
	}
	finish := domain.FinishStop
	term := Chunk{ID: "bench", Model: req.Model, FinishReason: &finish}
	acc.ObserveChunk(term)
	if handler != nil {
		if err := handler(term); err != nil {
			return nil, newStreamError(err)
		}
	}
	return acc.Response(), nil
}

func benchReq() *Request {
	return &Request{Model: "bench-model", Params: &domain.ChatCompletionRequest{},
		Ref: domain.ModelRef{ProviderName: "bench", Model: "bench-model"}}
}

func repeatChunks(word string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = word + " "
	}
	return out
}

// measureFirstByte runs one stream and returns time-to-first-handler-call and
// total duration.
func measureFirstByte(a *scriptAdapter) (firstByte, total time.Duration, err error) {
	var first time.Time
	start := time.Now()
	firstSeen := false
	_, err = a.ChatCompletionStream(context.Background(), benchReq(), func(Chunk) error {
		if !firstSeen {
			firstSeen = true
			first = time.Now()
		}
		return nil
	})
	total = time.Since(start)
	if firstSeen {
		firstByte = first.Sub(start)
	}
	return firstByte, total, err
}

func BenchmarkStreamTTFT_Small(b *testing.B) {
	a := &scriptAdapter{name: "bench", firstDelay: 20 * time.Millisecond, textChunks: []string{"hello world"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		first, _, err := measureFirstByte(a)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(first)/float64(time.Second), "first-byte-s/op")
	}
}

func BenchmarkStreamThroughput_Medium(b *testing.B) {
	chunks := repeatChunks("token", 200)
	a := &scriptAdapter{name: "bench", textChunks: chunks}
	var totalBytes int64
	for _, c := range chunks {
		totalBytes += int64(len(c))
	}
	b.SetBytes(totalBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := measureFirstByte(a); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamThroughput_Long(b *testing.B) {
	chunks := repeatChunks("loremipsum", 2000)
	a := &scriptAdapter{name: "bench", textChunks: chunks}
	var totalBytes int64
	for _, c := range chunks {
		totalBytes += int64(len(c))
	}
	b.SetBytes(totalBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := measureFirstByte(a); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamConcurrent(b *testing.B) {
	a := &scriptAdapter{name: "bench", firstDelay: 5 * time.Millisecond, textChunks: repeatChunks("tok", 50)}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, _, err := measureFirstByte(a); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkStreamSlowConsumer(b *testing.B) {
	a := &scriptAdapter{name: "bench", textChunks: repeatChunks("tok", 100)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := a.ChatCompletionStream(context.Background(), benchReq(), func(Chunk) error {
			time.Sleep(100 * time.Microsecond) // 10k tok/s reader
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamCancelCleanup(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := &scriptAdapter{name: "bench", hang: true}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := a.ChatCompletionStream(ctx, benchReq(), func(Chunk) error { return nil })
			done <- err
		}()
		time.Sleep(5 * time.Millisecond)
		start := time.Now()
		cancel()
		err := <-done
		b.ReportMetric(float64(time.Since(start))/float64(time.Microsecond), "cleanup-us/op")
		if err == nil {
			b.Fatal("expected cancel error")
		}
	}
}

// TestScriptAdapterSanity guards the benchmark harness itself.
func TestScriptAdapterSanity(t *testing.T) {
	a := &scriptAdapter{name: "s", firstDelay: 10 * time.Millisecond, textChunks: []string{"a", "b"}}
	first, total, err := measureFirstByte(a)
	if err != nil {
		t.Fatal(err)
	}
	if first < 10*time.Millisecond || first > total {
		t.Fatalf("first=%v total=%v", first, total)
	}
	fail := &scriptAdapter{name: "s", textChunks: []string{"a", "b"}, failAfter: 1}
	var n int
	_, err = fail.ChatCompletionStream(context.Background(), benchReq(), func(Chunk) error { n++; return nil })
	if err == nil || n != 1 {
		t.Fatalf("err=%v chunks=%d", err, n)
	}
}
