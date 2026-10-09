package routing

import (
	"context"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

func cancelExecutor(t *testing.T) (*Executor, *domain.RequestContext) {
	t.Helper()
	models := []domain.Model{testModel(openaiID, "openai", "gpt-4o")}
	provider := testProvider(openaiID, "openai", domain.ProviderOpenAI, 1)
	engine := NewEngine(NewStaticCatalogue(models, []domain.Provider{provider}), nil, nil)
	rc := resolveWith(t, engine, testRequest("gpt-4o"))
	return nil, rc
}

func cancelRegistry(provider domain.Provider, adapter *stubAdapter) *providers.Registry {
	return newRegistry(struct {
		provider domain.Provider
		adapter  *stubAdapter
	}{provider: provider, adapter: adapter})
}

// TestExecutorCancelBeforeFirstByte is retryable: nothing reached the client,
// so the error must not be a StreamStartedError.
func TestExecutorCancelBeforeFirstByte(t *testing.T) {
	_, rc := cancelExecutor(t)
	provider := testProvider(openaiID, "openai", domain.ProviderOpenAI, 1)

	ctx, cancel := context.WithCancel(context.Background())
	adapter := newStubAdapter("openai", domain.ProviderOpenAI)
	adapter.onStream = func(handler providers.StreamHandler, req *providers.Request) error {
		// Never emit: the attempt is cancelled while waiting.
		<-ctx.Done()
		return ctx.Err()
	}
	executor := NewExecutor(cancelRegistry(provider, adapter), NewHealthTracker(HealthConfig{}))

	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(ctx, rc, &providers.Request{Model: "gpt-4o"},
			func(providers.Chunk) error { return nil })
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected cancel error")
		}
		if IsStreamStarted(err) {
			t.Fatalf("nothing was delivered; must not be stream-started: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Execute did not return after cancel")
	}
}

// TestExecutorCancelAfterFirstByte pins the stream: failover is impossible
// once bytes are delivered, and the error must say so.
func TestExecutorCancelAfterFirstByte(t *testing.T) {
	_, rc := cancelExecutor(t)
	provider := testProvider(openaiID, "openai", domain.ProviderOpenAI, 1)

	ctx, cancel := context.WithCancel(context.Background())
	adapter := newStubAdapter("openai", domain.ProviderOpenAI)
	adapter.onStream = func(handler providers.StreamHandler, req *providers.Request) error {
		if err := handler(providers.Chunk{ID: "c1", Model: req.Model}); err != nil {
			return err
		}
		// Wait for cancellation instead of completing: the client went away
		// mid-stream.
		<-ctx.Done()
		return ctx.Err()
	}
	executor := NewExecutor(cancelRegistry(provider, adapter), NewHealthTracker(HealthConfig{}))

	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(ctx, rc, &providers.Request{Model: "gpt-4o"},
			func(providers.Chunk) error { return nil })
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected error after cancel")
		}
		if !IsStreamStarted(err) {
			t.Fatalf("bytes were delivered; must be stream-started: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Execute did not return after cancel")
	}
}
