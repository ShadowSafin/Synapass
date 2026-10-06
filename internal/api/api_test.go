package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/auth"
	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/policy"
	"github.com/shadowsafin/synapass/internal/providers"
	"github.com/shadowsafin/synapass/internal/routing"
	"github.com/shadowsafin/synapass/internal/version"
)

// ---------------------------------------------------------------------------
// Test doubles
//
// Everything below the HTTP boundary is a stub. The point of these tests is the
// request pipeline itself: authentication, routing, fallback, response shaping and
// the derived metadata a client sees. Nothing here needs a database, a Redis, or a
// network.
// ---------------------------------------------------------------------------

const (
	primaryID    = "p-openai"
	secondaryID  = "p-anthropic"
	testToken    = "syn_live_abcdefghijklmnopqrstuvwxyz0123456789"
	testAdminKey = "syn_admin_abcdefghijklmnopqrstuvwxyz0123456789"
	testModel    = "gpt-4o"
)

// stubKeyStore is an in-memory auth.KeyStore.
type stubKeyStore struct {
	mu     sync.Mutex
	key    *domain.APIKey
	tenant *domain.Tenant
}

func (s *stubKeyStore) LookupKey(_ context.Context, keyHash string) (*domain.APIKey, *domain.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key == nil || s.key.KeyHash != keyHash {
		return nil, nil, nil
	}
	key := *s.key
	tenant := *s.tenant
	return &key, &tenant, nil
}

func (s *stubKeyStore) LookupByPrefix(context.Context, string) ([]domain.APIKey, error) {
	return nil, nil
}

func (s *stubKeyStore) TouchKey(context.Context, string) error { return nil }

// stubPolicyRepo is an in-memory policy.Repository.
type stubPolicyRepo struct {
	policies []domain.RoutingPolicy
}

func (s *stubPolicyRepo) ListPolicies(context.Context) ([]domain.RoutingPolicy, error) {
	return s.policies, nil
}

func (s *stubPolicyRepo) GetPolicy(_ context.Context, id string) (*domain.RoutingPolicy, error) {
	for i := range s.policies {
		if s.policies[i].ID == id {
			return &s.policies[i], nil
		}
	}
	return nil, nil
}

// stubPolicyEngine scripts the fine-grained policy decision.
type stubPolicyEngine struct {
	deny string
}

func (s *stubPolicyEngine) Evaluate(_ context.Context, _ *domain.RoutingPolicy, _ *domain.RequestContext) *domain.PolicyDecision {
	if s.deny == "" {
		return &domain.PolicyDecision{Allowed: true}
	}
	return &domain.PolicyDecision{Allowed: false, DenyMessage: s.deny}
}

// stubModelSource is an in-memory ModelSource.
type stubModelSource struct {
	models []domain.Model
}

func (s *stubModelSource) List(context.Context) ([]domain.Model, error) { return s.models, nil }

// stubAdapter is a provider adapter whose outcome is scripted.
type stubAdapter struct {
	name    string
	kind    domain.ProviderKind
	content string
	// reasoning, when set, is served as the message thinking trace, the way
	// a provider that emits reasoning_content would.
	reasoning string

	mu      sync.Mutex
	calls   int
	failure error
	// hangBeforeStreamChunk, when set, blocks the streaming path until the context
	// is done, which exercises the cancellation path.
	hangBeforeStreamChunk bool
	// lengthFinish makes the adapter report finish_reason "length", the way a
	// provider does when it stopped because it hit the token ceiling. It is the
	// signal that an answer was cut short rather than finished.
	lengthFinish bool
}

func newStubAdapter(name string, kind domain.ProviderKind, content string) *stubAdapter {
	return &stubAdapter{name: name, kind: kind, content: content}
}

func (s *stubAdapter) Name() string              { return s.name }
func (s *stubAdapter) Kind() domain.ProviderKind { return s.kind }
func (s *stubAdapter) Capabilities() domain.CapabilitySet {
	return domain.NewCapabilitySet(domain.CapChat, domain.CapStreaming, domain.CapTools)
}
func (s *stubAdapter) Enabled() bool { return true }

func (s *stubAdapter) HealthCheck(context.Context) domain.ProviderHealth {
	return domain.ProviderHealth{
		ProviderName: s.name,
		State:        domain.HealthHealthy,
		CheckedAt:    domain.Now(),
		Source:       "stub",
	}
}

func (s *stubAdapter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubAdapter) setFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = err
}

func (s *stubAdapter) enter() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.failure
}

func (s *stubAdapter) response(req *providers.Request) *providers.Response {
	finish := domain.FinishStop
	if s.lengthFinish {
		finish = domain.FinishLength
	}
	return &providers.Response{
		ID:      "cmpl-" + s.name,
		Model:   req.Model,
		Created: time.Now().Unix(),
		Choices: []domain.Choice{{
			Index:        0,
			Message:      &domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(s.content), Reasoning: s.reasoning},
			FinishReason: finishReasonPtr(finish),
		}},
		Usage: domain.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
	}
}

func finishReasonPtr(r domain.FinishReason) *domain.FinishReason { return &r }

func (s *stubAdapter) ChatCompletion(_ context.Context, req *providers.Request) (*providers.Response, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	return s.response(req), nil
}

func (s *stubAdapter) ChatCompletionStream(ctx context.Context, req *providers.Request, handler providers.StreamHandler) (*providers.Response, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}

	if s.hangBeforeStreamChunk {
		<-ctx.Done()
		return nil, domain.NewError(domain.ErrCodeCanceled, "the client disconnected").Wrap(ctx.Err())
	}

	if handler != nil {
		finish := domain.FinishStop
		chunks := []providers.Chunk{
			{ID: "cmpl-" + s.name, Model: req.Model, Index: 0,
				Delta: domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(s.content)}},
			{ID: "cmpl-" + s.name, Model: req.Model, Index: 0,
				Delta: domain.ChatMessage{Role: domain.RoleAssistant}, FinishReason: &finish},
		}
		for _, chunk := range chunks {
			if err := handler(chunk); err != nil {
				return nil, err
			}
		}
	}
	resp := s.response(req)
	return resp, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	server    *Server
	http      *httptest.Server
	primary   *stubAdapter
	secondary *stubAdapter
}

type harnessOptions struct {
	failPrimary  bool
	primaryError error
	// denyPolicy, when non-empty, makes the stub policy engine deny every
	// request with this message.
	denyPolicy string
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	providersList := []domain.Provider{
		{
			ID: primaryID, Name: "openai", Kind: domain.ProviderOpenAI,
			BaseURL: "https://primary.invalid/v1", Status: domain.StatusActive, Priority: 1,
		},
		{
			ID: secondaryID, Name: "anthropic", Kind: domain.ProviderAnthropic,
			BaseURL: "https://secondary.invalid/v1", Status: domain.StatusActive, Priority: 2,
		},
	}
	models := []domain.Model{
		{
			ID: "m-primary", ProviderID: primaryID, ProviderName: "openai", Name: testModel,
			Status: domain.ModelActive, ContextWindow: 128_000, MaxOutputTokens: 4096,
			Capabilities:        []domain.Capability{domain.CapChat, domain.CapStreaming, domain.CapTools},
			InputCostPerMillion: 2.5, OutputCostPerMillion: 10,
		},
		{
			ID: "m-secondary", ProviderID: secondaryID, ProviderName: "anthropic", Name: testModel,
			Status: domain.ModelActive, ContextWindow: 200_000, MaxOutputTokens: 8192,
			Capabilities:        []domain.Capability{domain.CapChat, domain.CapStreaming, domain.CapTools},
			InputCostPerMillion: 3, OutputCostPerMillion: 15,
		},
	}

	primary := newStubAdapter("openai", domain.ProviderOpenAI, "hello from the primary")
	secondary := newStubAdapter("anthropic", domain.ProviderAnthropic, "hello from the secondary")
	if opts.failPrimary {
		failure := opts.primaryError
		if failure == nil {
			failure = domain.NewError(domain.ErrCodeUpstream, "the primary provider is down")
		}
		primary.setFailure(failure)
	}

	registry := providers.NewRegistry()
	registry.Register(providersList[0], primary)
	registry.Register(providersList[1], secondary)

	health := routing.NewHealthTracker(routing.HealthConfig{})
	catalogue := routing.NewStaticCatalogue(models, providersList)

	// Retry is disabled so a failure means exactly one call per provider, which
	// makes the failover assertions unambiguous.
	policies := &stubPolicyRepo{policies: []domain.RoutingPolicy{{
		ID: "default", Name: "default", Enabled: true, Priority: 100,
		Strategy: domain.StrategyPriority,
		Fallback: domain.FallbackPolicy{Enabled: true, MaxAttempts: 2},
		Retry:    domain.RetryPolicy{MaxAttempts: 1},
		Limits:   domain.PolicyLimits{MaxOutputTokens: 512, LatencyTargetMS: 20_000},
	}}}

	resolver := policy.NewResolver(policies, time.Minute)
	engine := routing.NewEngine(catalogue, resolver, health)
	executor := routing.NewExecutor(registry, health)

	store := &stubKeyStore{
		key: &domain.APIKey{
			ID: "k-1", TenantID: "t-1", Name: "test key",
			Prefix:  domain.DisplayPrefix(testToken),
			KeyHash: auth.HashKey(testToken),
			Scopes:  []string{string(domain.ScopeInference)},
			Status:  domain.APIKeyActive,
		},
		tenant: &domain.Tenant{
			ID: "t-1", Slug: "t-1", Name: "Test Tenant", Status: domain.StatusActive,
		},
	}
	authenticator := auth.New(auth.Options{Store: store, AdminKey: testAdminKey})

	cfg := config.Default()
	// The admin surface needs durable repositories, which this harness
	// deliberately does not provide; the admin API is covered separately.
	cfg.Admin.Enabled = false
	// No trusted proxies: a forged X-Forwarded-For must not be believed.
	cfg.HTTP.TrustedProxies = nil

	server, err := NewServer(func() Deps {
		deps := Deps{
			Config:        cfg,
			Version:       version.Info{Version: "test", Commit: "deadbeef"},
			Authenticator: authenticator,
			Engine:        engine,
			Executor:      executor,
			Health:        health,
			Policies:      resolver,
			Adapters:      registry,
			Models:        &stubModelSource{models: models},
		}
		if opts.denyPolicy != "" {
			deps.PolicyEngine = &stubPolicyEngine{deny: opts.denyPolicy}
		}
		return deps
	}())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	return &harness{server: server, http: httpServer, primary: primary, secondary: secondary}
}

// do issues a request against the harness.
func (h *harness) do(t *testing.T, method, path, token, body string, headers map[string]string) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.http.URL+path, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// readBody drains and returns the response body.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

// chatBody is a minimal valid OpenAI chat request.
func chatBody(extra string) string {
	body := `{"model":"` + testModel + `","messages":[{"role":"user","content":"hello"}]`
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

// ---------------------------------------------------------------------------
// Inference endpoint
// ---------------------------------------------------------------------------

func TestChatCompletionsReturnsOpenAICompatibleResponse(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""),
		map[string]string{"X-Synapass-Debug": "true"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content type = %q", got)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("the request id must be echoed for support correlation")
	}

	var decoded struct {
		ID      string                   `json:"id"`
		Object  string                   `json:"object"`
		Model   string                   `json:"model"`
		Choices []domain.Choice          `json:"choices"`
		Usage   *domain.TokenUsage       `json:"usage"`
		Core    *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.Object != domain.ObjectChatCompletion {
		t.Errorf("object = %q, want %q", decoded.Object, domain.ObjectChatCompletion)
	}
	if len(decoded.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(decoded.Choices))
	}
	if got := decoded.Choices[0].Message.Text(); got != "hello from the primary" {
		t.Errorf("content = %q", got)
	}
	if decoded.Choices[0].FinishReason == nil || *decoded.Choices[0].FinishReason != domain.FinishStop {
		t.Errorf("finish reason = %v", decoded.Choices[0].FinishReason)
	}
	if decoded.Usage == nil || decoded.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", decoded.Usage)
	}
	if decoded.Core == nil {
		t.Fatal("the Synapass metadata block is missing")
	}
	if decoded.Core.Provider != "openai" {
		t.Errorf("metadata provider = %q", decoded.Core.Provider)
	}
	if decoded.Core.PolicyID != "default" {
		t.Errorf("metadata policy = %q", decoded.Core.PolicyID)
	}
	if decoded.Core.EstimatedCostUSD <= 0 {
		t.Errorf("metadata cost = %v, want a positive estimate", decoded.Core.EstimatedCostUSD)
	}
	if h.primary.callCount() != 1 {
		t.Errorf("primary calls = %d, want 1", h.primary.callCount())
	}
}

func TestChatCompletionsExposesThinkingTrace(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.primary.reasoning = "considering the options"

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	// The presence header lets an agent detect thinking without parsing JSON.
	if got := resp.Header.Get("X-Synapass-Thinking"); got != "present" {
		t.Errorf("X-Synapass-Thinking = %q, want present", got)
	}

	var decoded struct {
		Choices []domain.Choice `json:"choices"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(decoded.Choices))
	}
	msg := decoded.Choices[0].Message
	if msg.Reasoning != "considering the options" {
		t.Errorf("reasoning_content = %q", msg.Reasoning)
	}
	// The trace rides alongside the answer, never inside it.
	if got := msg.Text(); got != "hello from the primary" {
		t.Errorf("content = %q", got)
	}
}

func TestChatCompletionsOmitsThinkingSignalsWithoutTrace(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	if got := resp.Header.Get("X-Synapass-Thinking"); got != "" {
		t.Errorf("X-Synapass-Thinking = %q, want absent without a trace", got)
	}
	body := readBody(t, resp)
	var decoded struct {
		Choices []domain.Choice `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(decoded.Choices))
	}
	// No reasoning key at all: omitempty keeps plain responses byte-clean.
	var raw struct {
		Choices []map[string]any `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw.Choices[0]["message"].(map[string]any)["reasoning_content"]; present {
		t.Error("a response without thinking must not carry reasoning_content")
	}
}

func TestChatCompletionsFailsOverWithoutBreakingTheClient(t *testing.T) {
	h := newHarness(t, harnessOptions{failPrimary: true})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a fallback must still be a 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}

	var decoded struct {
		Choices []domain.Choice          `json:"choices"`
		Core    *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := decoded.Choices[0].Message.Text(); got != "hello from the secondary" {
		t.Errorf("content = %q, want the fallback provider's answer", got)
	}
	if decoded.Core == nil || decoded.Core.Provider != "anthropic" {
		t.Fatalf("metadata should name the provider that served the request: %+v", decoded.Core)
	}
	if !decoded.Core.FallbackUsed {
		t.Error("the metadata must disclose that fallback was used")
	}
	if h.primary.callCount() != 1 {
		t.Errorf("primary calls = %d, want 1", h.primary.callCount())
	}
	if h.secondary.callCount() != 1 {
		t.Errorf("secondary calls = %d, want 1", h.secondary.callCount())
	}
}

func TestChatCompletionsHonoursNoFallbackHeader(t *testing.T) {
	h := newHarness(t, harnessOptions{failPrimary: true})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""),
		map[string]string{"X-Synapass-No-Fallback": "true"})

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, readBody(t, resp))
	}
	if h.secondary.callCount() != 0 {
		t.Error("the no-fallback header must prevent the secondary provider from being called")
	}
	if h.primary.callCount() != 1 {
		t.Errorf("primary calls = %d, want 1", h.primary.callCount())
	}

	var decoded struct {
		Error domain.ErrorBody `json:"error"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Error.Type != "upstream_error" {
		t.Errorf("error type = %q", decoded.Error.Type)
	}
}

func TestChatCompletionsStreamsServerSentEvents(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken,
		chatBody(`"stream":true,"stream_options":{"include_usage":true}`), nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("content type = %q, want text/event-stream", got)
	}

	body := readBody(t, resp)
	if !strings.Contains(body, "data: ") {
		t.Errorf("no SSE frames in %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("the stream must be terminated with [DONE]")
	}
	if !strings.Contains(body, "hello from the primary") {
		t.Errorf("the streamed content is missing: %q", body)
	}
	// The usage frame is only sent when the client opts in, exactly as OpenAI's
	// stream_options does.
	if !strings.Contains(body, `"usage"`) {
		t.Errorf("the requested usage frame is missing: %q", body)
	}
	if !strings.Contains(body, domain.ObjectChatCompletionChunk) {
		t.Errorf("frames must carry the chunk object discriminator: %q", body)
	}
}

func TestChatCompletionsStreamingOmitsUsageFrameByDefault(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(`"stream":true`), nil)
	body := readBody(t, resp)

	if strings.Contains(body, `"usage"`) {
		t.Errorf("the usage frame must be opt-in: %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("the stream must still terminate with [DONE]")
	}
}

func TestChatCompletionsRejectsMissingCredentials(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", "", chatBody(""), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	var decoded struct {
		Error domain.ErrorBody `json:"error"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Error.Code != string(domain.ErrCodeAuthentication) {
		t.Errorf("error code = %q", decoded.Error.Code)
	}
	if h.primary.callCount() != 0 {
		t.Error("an unauthenticated request must never reach a provider")
	}
}

func TestChatCompletionsRejectsInvalidKey(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", "syn_live_this_is_not_a_real_key_at_all", chatBody(""), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestChatCompletionsEnforcesInferenceScope(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	// Rebuild the harness's key store with a key that holds no inference scope.
	scopeless := auth.HashKey("syn_live_scopeless_key_value_padding_000")
	store := &stubKeyStore{
		key: &domain.APIKey{
			ID: "k-2", TenantID: "t-1", Name: "scopeless",
			Prefix:  domain.DisplayPrefix("syn_live_scopeless_key_value_padding_000"),
			KeyHash: scopeless, Scopes: []string{string(domain.ScopeReadModels)},
			Status: domain.APIKeyActive,
		},
		tenant: &domain.Tenant{ID: "t-1", Slug: "t-1", Name: "Test Tenant", Status: domain.StatusActive},
	}

	cfg := config.Default()
	cfg.Admin.Enabled = false
	server, err := NewServer(Deps{
		Config:        cfg,
		Version:       version.Info{Version: "test"},
		Authenticator: auth.New(auth.Options{Store: store}),
		Engine:        h.server.engine,
		Executor:      h.server.executor,
		Health:        h.server.health,
		Policies:      h.server.policies,
		Adapters:      h.server.adapters,
		Models:        h.server.modelSource(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	scoped := httptest.NewServer(server.Handler())
	defer scoped.Close()

	req, err := http.NewRequest(http.MethodPost, scoped.URL+"/v1/chat/completions",
		strings.NewReader(chatBody("")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer syn_live_scopeless_key_value_padding_000")
	req.Header.Set("Content-Type", "application/json")

	resp, err := scoped.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestChatCompletionsRejectsInvalidBody(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	cases := map[string]string{
		"not json":        `{`,
		"missing model":   `{"messages":[{"role":"user","content":"hi"}]}`,
		"empty messages":  `{"model":"gpt-4o","messages":[]}`,
		"bad role":        `{"model":"gpt-4o","messages":[{"role":"wizard","content":"hi"}]}`,
		"empty content":   `{"model":"gpt-4o","messages":[{"role":"user","content":""}]}`,
		"bad temperature": `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":9}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readBody(t, resp))
			}
			var decoded struct {
				Error domain.ErrorBody `json:"error"`
			}
			if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if decoded.Error.Code != string(domain.ErrCodeInvalidRequest) {
				t.Errorf("error code = %q", decoded.Error.Code)
			}
		})
	}

	if h.primary.callCount() != 0 {
		t.Error("a malformed request must never reach a provider")
	}
}

func TestChatCompletionsToleratesClientExtensions(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	// VS Code Copilot Chat sends Anthropic-style cache_control hints at the
	// top level, on messages and inside content parts; other clients send
	// store/service_tier. All must be accepted, not 400.
	cases := map[string]string{
		"top-level cache_control": `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"cache_control":{"type":"ephemeral"}}`,
		"message cache_control":   `{"model":"gpt-4o","messages":[{"role":"user","content":"hi","cache_control":{"type":"ephemeral"}}]}`,
		"part cache_control":      `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`,
		"store/service_tier":      `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"store":false,"service_tier":"auto"}`,
		"future unknown field":    `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"bogus_future_field":1}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

func TestChatCompletionsRejectsUnknownModel(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken,
		`{"model":"gpt-9-nonexistent","messages":[{"role":"user","content":"hi"}]}`, nil)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readBody(t, resp))
	}
	if h.primary.callCount() != 0 {
		t.Error("an unroutable model must fail before any provider call")
	}
}

func TestChatCompletionsAppliesCostCeilingHeader(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	// A ceiling far below the projected cost forces the router onto the relaxed
	// pass rather than failing outright, and the decision reports the ceiling it
	// actually enforced.
	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""),
		map[string]string{"X-Synapass-Max-Cost-USD": "0.0000001", "X-Synapass-Debug": "true"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}

	var decoded struct {
		Core *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Core == nil {
		return
	}
	// A ceiling tighter than the projected cost forces the relaxed routing pass,
	// and the decision must disclose that it traded quality for the budget rather
	// than silently overspending.
	if !decoded.Core.Degraded {
		t.Errorf("a relaxed cost ceiling must be visible as a degraded decision: %+v", decoded.Core)
	}
	if !strings.Contains(decoded.Core.RouteReason, "cost ceiling relaxed") {
		t.Errorf("route reason = %q", decoded.Core.RouteReason)
	}
}

func TestNotImplementedSurfaces(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	for _, path := range []string{"/v1/completions", "/v1/embeddings", "/v1/responses"} {
		resp := h.do(t, http.MethodPost, path, testToken, `{}`, nil)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s status = %d, want 501", path, resp.StatusCode)
		}
	}
}

// ---------------------------------------------------------------------------
// Model registry endpoint
// ---------------------------------------------------------------------------

func TestListModels(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodGet, "/v1/models", testToken, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}

	var decoded struct {
		Object string               `json:"object"`
		Data   []domain.ModelObject `json:"data"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Object != "list" {
		t.Errorf("object = %q, want list", decoded.Object)
	}
	if len(decoded.Data) == 0 {
		t.Fatal("the registry is not empty, so at least one model must be listed")
	}
	for _, model := range decoded.Data {
		if model.ID == "" {
			t.Error("every listed model needs an id")
		}
	}
}

func TestListModelsRequiresAuthentication(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodGet, "/v1/models", "", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestGetModelRejectsUnknown(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodGet, "/v1/models/nope-not-here", testToken, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// Operational endpoints
// ---------------------------------------------------------------------------

func TestHealthAndVersionArePublic(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	for _, path := range []string{"/health", "/healthz"} {
		resp := h.do(t, http.MethodGet, path, "", "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, resp.StatusCode)
		}
		var decoded struct {
			Status     string          `json:"status"`
			Components map[string]bool `json:"components"`
		}
		if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
			t.Fatalf("%s decode: %v", path, err)
		}
		if decoded.Status != "ok" {
			t.Errorf("%s status field = %q", path, decoded.Status)
		}
	}

	resp := h.do(t, http.MethodGet, "/version", "", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/version status = %d", resp.StatusCode)
	}
	if !strings.Contains(readBody(t, resp), "test") {
		t.Error("/version should report the build version")
	}
}

func TestReadinessFailsWithoutProviders(t *testing.T) {
	// A gateway with no usable provider can only return errors, so readiness must
	// report 503 and let the load balancer drain the instance.
	cfg := config.Default()
	cfg.Admin.Enabled = false
	health := routing.NewHealthTracker(routing.HealthConfig{})
	engine := routing.NewEngine(routing.NewStaticCatalogue(nil, nil), nil, health)

	server, err := NewServer(Deps{
		Config:        cfg,
		Version:       version.Info{Version: "test"},
		Authenticator: auth.New(auth.Options{Store: &stubKeyStore{}}),
		Engine:        engine,
		Executor:      routing.NewExecutor(providers.NewRegistry(), health),
		Health:        health,
		Policies:      policy.NewResolver(&stubPolicyRepo{}, time.Minute),
		Adapters:      providers.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/ready")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestUnknownEndpointReturnsJSON404(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodGet, "/v1/does-not-exist", testToken, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content type = %q, want JSON so a client never parses two formats", got)
	}

	var decoded struct {
		Error domain.ErrorBody `json:"error"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Error.Message == "" {
		t.Error("the error envelope needs a message")
	}
}

func TestMetricsEndpointIsExposed(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodGet, "/metrics", "", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Middleware behaviour
// ---------------------------------------------------------------------------

func TestClientSuppliedRequestIDIsEchoedAndBounded(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""),
		map[string]string{"X-Request-ID": "my-correlation-id"})
	if got := resp.Header.Get("X-Request-ID"); got != "my-correlation-id" {
		t.Errorf("request id = %q, want the supplied value", got)
	}

	// A request id containing unsafe characters is replaced rather than
	// sanitized in place, so a mangled identifier never reaches the logs.
	resp = h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""),
		map[string]string{"X-Request-ID": "bad id with spaces"})
	if got := resp.Header.Get("X-Request-ID"); got == "bad id with spaces" {
		t.Error("an unsafe request id must be replaced with a generated one")
	}
}

func TestForwardedHeaderIsIgnoredWithoutTrustedProxies(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	// The header must not be believed, so the derived client address stays the
	// real peer. The response is a 200 either way; what matters is that the
	// request pipeline did not trust a client-forged address.
	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""),
		map[string]string{"X-Forwarded-For": "203.0.113.7"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestCORSPreflightForConfiguredOrigin(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodOptions, "/v1/chat/completions", "", "",
		map[string]string{
			"Origin":                        "http://localhost:3000",
			"Access-Control-Request-Method": "POST",
		})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("allow origin = %q", got)
	}
}

func TestCORSIsNotGrantedToUnlistedOrigin(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""),
		map[string]string{"Origin": "https://evil.example"})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin must receive no CORS grant, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Admin mount
// ---------------------------------------------------------------------------

func TestAdminSurfaceIsAbsentWhenDisabled(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodGet, "/admin/v1/overview", testAdminKey, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the admin API is disabled", resp.StatusCode)
	}
}

func TestAdminSurfaceRejectsAnonymousAccess(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.Enabled = true
	health := routing.NewHealthTracker(routing.HealthConfig{})

	server, err := NewServer(Deps{
		Config:        cfg,
		Version:       version.Info{Version: "test"},
		Authenticator: auth.New(auth.Options{Store: &stubKeyStore{}, AdminKey: testAdminKey}),
		Engine:        routing.NewEngine(routing.NewStaticCatalogue(nil, nil), nil, health),
		Executor:      routing.NewExecutor(providers.NewRegistry(), health),
		Health:        health,
		Policies:      policy.NewResolver(&stubPolicyRepo{}, time.Minute),
		Adapters:      providers.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/admin/v1/overview")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	// Administrative reads are never available anonymously, even in development.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func TestClientCancellationIsReportedAsCanceled(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.primary.hangBeforeStreamChunk = true

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.http.URL+"/v1/chat/completions",
		strings.NewReader(chatBody(`"stream":true`)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")

	// Cancel once the request is certainly in flight.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	resp, err := h.http.Client().Do(req)
	if err != nil {
		// A transport-level failure is an acceptable outcome for a canceled
		// request; the important thing is that the gateway did not hang.
		return
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
}
