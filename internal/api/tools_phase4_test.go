package api

import (
	"context"
	"encoding/json"
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
	"github.com/shadowsafin/synapass/internal/tools"
	"github.com/shadowsafin/synapass/internal/version"
)

// ---------------------------------------------------------------------------
// Phase 4: tool calling over HTTP.
//
// These tests drive the real router with a scripted tool-calling provider, so
// what is asserted is the wire behaviour a client sees: tool declarations
// accepted, tool calls passed through untouched, client-side execution
// unchanged by default, and the gateway-side loop bounded and recorded.
// ---------------------------------------------------------------------------

// toolProvider is a provider that emits a tool call on the first turn and an
// answer on every later turn. It also records the messages each turn saw, which
// is how the tests prove a tool result really was injected.
type toolProvider struct {
	name     string
	kind     domain.ProviderKind
	toolCall domain.ToolCall
	answer   string
	// plainAnswer, when set, makes turn one ordinary text. The tool tests need a
	// tool call on the first turn; the structured-output tests need prose.
	plainAnswer string
	// loopForever makes every turn a tool call, which is what a model does when
	// it keeps asking for the same tool. The policy bound is what has to stop it.
	loopForever bool

	mu        sync.Mutex
	turns     [][]domain.ChatMessage
	seenTools [][]domain.Tool
	refusals  []string
}

func (p *toolProvider) Name() string              { return p.name }
func (p *toolProvider) Kind() domain.ProviderKind { return p.kind }
func (p *toolProvider) Enabled() bool             { return true }
func (p *toolProvider) Capabilities() domain.CapabilitySet {
	return domain.NewCapabilitySet(domain.CapChat, domain.CapTools,
		domain.CapJSONMode, domain.CapStreaming, domain.CapJSONSchema)
}
func (p *toolProvider) HealthCheck(context.Context) domain.ProviderHealth {
	return domain.ProviderHealth{
		ProviderName: p.name, State: domain.HealthHealthy,
		CheckedAt: domain.Now(), Source: "stub",
	}
}

func (p *toolProvider) ChatCompletion(
	_ context.Context, req *providers.Request,
) (*providers.Response, error) {
	p.mu.Lock()
	p.turns = append(p.turns, append([]domain.ChatMessage(nil), req.Params.Messages...))
	p.seenTools = append(p.seenTools, append([]domain.Tool(nil), req.Params.Tools...))
	turn := len(p.turns)
	p.mu.Unlock()

	if (turn == 1 || p.loopForever) && p.plainAnswer == "" {
		finish := domain.FinishToolCalls
		return &providers.Response{
			ID:    "cmpl-tool-1",
			Model: req.Model,
			Choices: []domain.Choice{{
				Index: 0,
				Message: &domain.ChatMessage{
					Role:      domain.RoleAssistant,
					Content:   domain.NewTextContent("let me check"),
					ToolCalls: []domain.ToolCall{p.toolCall},
				},
				FinishReason: &finish,
			}},
			Usage: domain.TokenUsage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
		}, nil
	}

	answer := p.answer
	if turn == 1 {
		answer = p.plainAnswer
	}
	finish := domain.FinishStop
	return &providers.Response{
		ID:      "cmpl-tool-2",
		Model:   req.Model,
		Created: time.Now().Unix(),
		Choices: []domain.Choice{{
			Index: 0,
			Message: &domain.ChatMessage{
				Role:    domain.RoleAssistant,
				Content: domain.NewTextContent(answer),
			},
			FinishReason: &finish,
		}},
		Usage: domain.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
	}, nil
}

func (p *toolProvider) ChatCompletionStream(
	ctx context.Context, req *providers.Request, handler providers.StreamHandler,
) (*providers.Response, error) {
	resp, err := p.ChatCompletion(ctx, req)
	if err != nil {
		return nil, err
	}
	if handler != nil {
		if len(resp.Choices) > 0 && resp.Choices[0].Message != nil {
			if herr := handler(providers.Chunk{
				ID:           resp.ID,
				Model:        req.Model,
				Delta:        domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent("ok")},
				Created:      resp.Created,
				Index:        0,
				FinishReason: resp.Choices[0].FinishReason,
			}); herr != nil {
				return nil, herr
			}
		}
	}
	return resp, nil
}

func (p *toolProvider) seenTurns() [][]domain.ChatMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.turns
}

func (p *toolProvider) observedTools() [][]domain.Tool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seenTools
}

// toolRegistry is an in-memory RegistrySource.
type toolRegistry struct{ specs []domain.ToolSpec }

func (r *toolRegistry) List(context.Context) ([]domain.ToolSpec, error) { return r.specs, nil }

// toolPolicySource is an in-memory PolicySource.
type toolPolicySource struct{ policy *domain.ToolPolicy }

func (s *toolPolicySource) ResolveFor(context.Context, string) (*domain.ToolPolicy, error) {
	if s.policy == nil {
		return nil, nil
	}
	return s.policy, nil
}

// invocationStore records what the pipeline wrote.
type invocationStore struct {
	mu          sync.Mutex
	invocations []domain.ToolInvocation
	executions  []domain.ToolExecution
}

func (s *invocationStore) InsertInvocation(_ context.Context, invocation *domain.ToolInvocation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invocations = append(s.invocations, *invocation)
	return nil
}

func (s *invocationStore) InsertExecution(_ context.Context, execution *domain.ToolExecution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executions = append(s.executions, *execution)
	return nil
}

// runStore records agent runs and steps.
//
// It also remembers whether the first step arrived after the first run row.
// That ordering is a real constraint, not a style preference: agent_steps has a
// foreign key to agent_runs, so this is the check that would catch a
// regression that loses every step trace while still reporting a completed run.
type runStore struct {
	mu    sync.Mutex
	runs  []tools.RunRecord
	steps []tools.RunStep

	runWrites              int
	firstStepAfterFirstRun bool
}

func (s *runStore) UpsertRun(_ context.Context, run tools.RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, run)
	s.runWrites++
	return nil
}

func (s *runStore) InsertRunStep(_ context.Context, _ string, step tools.RunStep) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.steps) == 0 {
		s.firstStepAfterFirstRun = s.runWrites > 0
	}
	s.steps = append(s.steps, step)
	return nil
}

// toolHarness is a self-contained server for the tool tests.
type toolHarness struct {
	http        *httptest.Server
	provider    *toolProvider
	invocations *invocationStore
	runs        *runStore
}

func newToolHarness(t *testing.T, toolPolicy *domain.ToolPolicy, specs []domain.ToolSpec) *toolHarness {
	return newToolHarnessAnswering(t, toolPolicy, specs, "")
}

// newLoopingHarness builds a harness whose provider asks for a tool on every
// turn, so a policy bound is the only thing that can end the run.
func newLoopingHarness(t *testing.T) *toolHarness {
	h := newToolHarnessAnswering(t, automaticPolicy(), []domain.ToolSpec{nowSpec()}, "")
	h.provider.loopForever = true
	return h
}

// newToolHarnessAnswering builds a harness whose first turn is prose rather
// than a tool call, for requests that involve no tools. Gateway-side execution
// is on, preserving the Phase 4 opt-in behaviour these tests cover.
func newToolHarnessAnswering(
	t *testing.T, toolPolicy *domain.ToolPolicy, specs []domain.ToolSpec, plainAnswer string,
) *toolHarness {
	return buildToolHarness(t, toolPolicy, specs, plainAnswer, true)
}

// newClientHarness builds a harness with gateway-side execution disabled: the
// production default. Automatic requests are clamped to manual, registry tools
// are never advertised, and the client always runs its own tools.
func newClientHarness(
	t *testing.T, toolPolicy *domain.ToolPolicy, specs []domain.ToolSpec, plainAnswer string,
) *toolHarness {
	return buildToolHarness(t, toolPolicy, specs, plainAnswer, false)
}

func buildToolHarness(
	t *testing.T, toolPolicy *domain.ToolPolicy, specs []domain.ToolSpec, plainAnswer string,
	gatewayExec bool,
) *toolHarness {
	t.Helper()

	const providerID = "p-tool"
	const providerName = "toolprov"

	providerConfig := domain.Provider{
		ID: providerID, Name: providerName, Kind: domain.ProviderOpenAICompatible,
		BaseURL: "https://tool.invalid/v1", Status: domain.StatusActive, Priority: 1,
		Capabilities: []domain.Capability{domain.CapChat, domain.CapTools, domain.CapJSONMode, domain.CapStreaming, domain.CapJSONSchema},
	}
	models := []domain.Model{{
		ID: "m-tool", ProviderID: providerID, ProviderName: providerName,
		Name: testModel, Status: domain.ModelActive, ContextWindow: 128000,
		MaxOutputTokens:     4096,
		Capabilities:        []domain.Capability{domain.CapChat, domain.CapTools, domain.CapJSONMode, domain.CapStreaming, domain.CapJSONSchema},
		InputCostPerMillion: 1, OutputCostPerMillion: 2,
	}}

	provider := &toolProvider{
		name:        providerName,
		kind:        domain.ProviderOpenAICompatible,
		answer:      "it is now",
		plainAnswer: plainAnswer,
		toolCall: domain.ToolCall{
			ID: "call_now_1",
			Function: domain.FunctionCall{
				Name:      "now",
				Arguments: `{}`,
			},
		},
	}

	registry := providers.NewRegistry()
	registry.Register(providerConfig, provider)

	health := routing.NewHealthTracker(routing.HealthConfig{})
	catalogue := routing.NewStaticCatalogue(models, []domain.Provider{providerConfig})
	policyRepo := &stubPolicyRepo{policies: []domain.RoutingPolicy{{
		ID: "default", Name: "default", Enabled: true, Priority: 100,
		Strategy: domain.StrategyPriority,
		Fallback: domain.FallbackPolicy{Enabled: true, MaxAttempts: 2},
		Retry:    domain.RetryPolicy{MaxAttempts: 1},
		Limits:   domain.PolicyLimits{MaxOutputTokens: 512},
	}}}
	resolver := policy.NewResolver(policyRepo, time.Minute)
	engine := routing.NewEngine(catalogue, resolver, health)
	executor := routing.NewExecutor(registry, health)

	store := &stubKeyStore{
		key: &domain.APIKey{
			ID: "k-tool", TenantID: "t-tool", Name: "tool key",
			Prefix:  domain.DisplayPrefix(testToken),
			KeyHash: auth.HashKey(testToken),
			Scopes:  []string{string(domain.ScopeInference)},
			Status:  domain.APIKeyActive,
		},
		tenant: &domain.Tenant{
			ID: "t-tool", Slug: "tool-tenant", Name: "Tool Tenant", Status: domain.StatusActive,
		},
	}

	cfg := config.Default()
	cfg.Admin.Enabled = false
	cfg.HTTP.TrustedProxies = nil
	cfg.Tools.GatewayExecution = gatewayExec

	invocations := &invocationStore{}
	runs := &runStore{}

	server, err := NewServer(Deps{
		Config:        cfg,
		Version:       version.Info{Version: "test", Commit: "deadbeef"},
		Authenticator: auth.New(auth.Options{Store: store, AdminKey: testAdminKey}),
		Engine:        engine,
		Executor:      executor,
		Health:        health,
		Policies:      resolver,
		Adapters:      registry,
		Models:        &stubModelSource{models: models},
		Tools: &ToolServices{
			Registry:    &toolRegistry{specs: specs},
			Policies:    &toolPolicySource{policy: toolPolicy},
			Invocations: invocations,
			Runs:        runs,
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	return &toolHarness{http: httpServer, provider: provider, invocations: invocations, runs: runs}
}

// post issues a chat request and returns the status and decoded envelope.
func (h *toolHarness) post(t *testing.T, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.http.URL+"/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	raw := readAll(t, resp)
	return resp.StatusCode, raw
}

// readAll drains a response body.
func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			return buf
		}
	}
}

// nowSpec is an executable built-in tool.
func nowSpec() domain.ToolSpec {
	return domain.ToolSpec{
		ID: "t-now", Name: "now", Kind: domain.ToolBuiltin, Owner: domain.OwnerPlatform,
		Description: "current time",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		SafetyLevel: domain.SafetySafe, Executable: true, Handler: "now",
		Enabled: true,
	}
}

// externalToolSpec is a discovery-only tool.
func externalToolSpec() domain.ToolSpec {
	spec := nowSpec()
	spec.ID = "t-ext"
	spec.Kind = domain.ToolExternal
	spec.Executable = false
	spec.SafetyLevel = domain.SafetySensitive
	return spec
}

func automaticPolicy() *domain.ToolPolicy {
	p := domain.DefaultToolPolicy()
	p.Mode = domain.ToolsAutomatic
	p.MaxSteps = 3
	return &p
}

const toolDeclaration = `"tools":[{"type":"function","function":{"name":"now","parameters":{"type":"object"}}}]`

// ---------------------------------------------------------------------------
// Request validation
// ---------------------------------------------------------------------------

func TestChatAcceptsToolDefinitions(t *testing.T) {
	h := newToolHarness(t, nil, []domain.ToolSpec{nowSpec()})
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_time","description":"now",
		"parameters":{"type":"object","properties":{}}}}]}`)
	if status != http.StatusOK {
		t.Fatalf("a valid tool declaration must be accepted, got %d: %s", status, raw)
	}
}

func TestChatRejectsMalformedTool(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing name", `"tools":[{"type":"function","function":{"description":"x"}}]`, "name"},
		{"duplicate", `"tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"a"}}]`, "more than once"},
		{"invalid name", `"tools":[{"type":"function","function":{"name":"has space"}}]`, "valid function name"},
		{"parameters not an object", `"tools":[{"type":"function","function":{"name":"a","parameters":"nope"}}]`, "not a JSON object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newToolHarness(t, nil, nil)
			status, raw := h.post(t,
				`{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],`+c.body+`}`)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", status, raw)
			}
			if !strings.Contains(string(raw), c.want) {
				t.Errorf("error should mention %q: %s", c.want, raw)
			}
		})
	}
}

func TestChatRejectsUnknownToolChoice(t *testing.T) {
	h := newToolHarness(t, nil, nil)
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"known"}}],
		"tool_choice":{"type":"function","function":{"name":"missing"}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, raw)
	}
	if !strings.Contains(string(raw), "missing") {
		t.Errorf("the error should name the missing tool: %s", raw)
	}
}

func TestChatRejectsUnknownToolExecutionMode(t *testing.T) {
	h := newToolHarness(t, nil, nil)
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"tool_execution":{"mode":"yolo"},"tools":[]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, raw)
	}
}

// ---------------------------------------------------------------------------
// Compatibility
// ---------------------------------------------------------------------------

func TestClientSideToolCallsAreUnchangedByDefault(t *testing.T) {
	// The compatibility guarantee: a client that sends tools and no execution
	// block gets the pre-Phase-4 behaviour, with the model's tool calls handed
	// back untouched for the client to run.
	h := newToolHarness(t, nil, []domain.ToolSpec{nowSpec()})
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"what time is it"}],`+
		toolDeclaration+`}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	var decoded struct {
		Choices []domain.Choice          `json:"choices"`
		Core    *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("the model's tool call must reach the client: %+v", decoded.Choices[0].Message)
	}
	if decoded.Choices[0].FinishReason == nil || *decoded.Choices[0].FinishReason != domain.FinishToolCalls {
		t.Errorf("finish reason = %v", decoded.Choices[0].FinishReason)
	}
	if len(h.invocations.executions) != 0 {
		t.Error("the default mode must not execute tools in the gateway")
	}
	if decoded.Core != nil && decoded.Core.ToolRun != nil &&
		decoded.Core.ToolRun.Status != domain.ToolRunClientExecuted {
		t.Errorf("tool run status = %q, want client_executed", decoded.Core.ToolRun.Status)
	}
}

// ---------------------------------------------------------------------------
// Gateway execution
// ---------------------------------------------------------------------------

func TestBoundedRunExplainsItself(t *testing.T) {
	// A live provider does this whenever the model keeps asking for the same
	// tool: the bound stops the run while its last move is still a tool call.
	// Returning that call to the client would be a dangling call, because the
	// tool result lives in the gateway's run, not in the caller's context.
	h := newLoopingHarness(t)
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"what time is it"}],
		"tool_execution":{"mode":"automatic"},`+toolDeclaration+`}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	var decoded struct {
		Choices []domain.Choice          `json:"choices"`
		Core    *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choice := decoded.Choices[0]

	if len(choice.Message.ToolCalls) != 0 {
		t.Errorf("a bounded run must not hand back an unsatisfiable tool call: %+v", choice.Message)
	}
	if choice.FinishReason == nil || *choice.FinishReason != domain.FinishStop {
		t.Errorf("finish reason = %v, want stop", choice.FinishReason)
	}

	// The notice has to say what happened, not merely that something did.
	answer := strings.ToLower(choice.Message.Text())
	for _, want := range []string{"3-step limit", "now", "executed"} {
		if !strings.Contains(answer, want) {
			t.Errorf("the notice should mention %q: %q", want, choice.Message.Text())
		}
	}
	if decoded.Core == nil || decoded.Core.ToolRun == nil {
		t.Fatal("the tool run must still be reported")
	}
	if decoded.Core.ToolRun.Status != domain.ToolRunGatewayExecuted {
		t.Errorf("status = %q", decoded.Core.ToolRun.Status)
	}
	if !strings.Contains(decoded.Core.ToolRun.StopReason, "step limit") {
		t.Errorf("stop reason = %q", decoded.Core.ToolRun.StopReason)
	}
}

func TestGatewayExecutesToolAndReturnsFinalAnswer(t *testing.T) {
	h := newToolHarness(t, automaticPolicy(), []domain.ToolSpec{nowSpec()})
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"what time is it"}],
		"tool_execution":{"mode":"automatic"},`+toolDeclaration+`}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	var decoded struct {
		Choices []domain.Choice          `json:"choices"`
		Core    *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The client gets one coherent final answer, not the intermediate tool call.
	if len(decoded.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("the final answer must not carry tool calls: %+v", decoded.Choices[0].Message)
	}
	if got := decoded.Choices[0].Message.Text(); got != "it is now" {
		t.Errorf("answer = %q", got)
	}
	if decoded.Choices[0].FinishReason == nil || *decoded.Choices[0].FinishReason != domain.FinishStop {
		t.Errorf("finish reason = %v", decoded.Choices[0].FinishReason)
	}

	if decoded.Core == nil || decoded.Core.ToolRun == nil {
		t.Fatal("the tool run must be reported in the response metadata")
	}
	run := decoded.Core.ToolRun
	if run.Status != domain.ToolRunGatewayExecuted {
		t.Errorf("tool run status = %q, want gateway_executed", run.Status)
	}
	if run.Executed != 1 || run.Calls != 1 || run.Steps != 2 {
		t.Errorf("executed = %d, calls = %d, steps = %d; want 1/1/2",
			run.Executed, run.Calls, run.Steps)
	}

	// The tool result must really have reached the model.
	turns := h.provider.seenTurns()
	if len(turns) < 2 {
		t.Fatalf("the model saw %d turns, want at least 2", len(turns))
	}
	second := turns[1]
	if len(second) < 3 {
		t.Fatalf("the second turn has %d messages: %+v", len(second), second)
	}
	last := second[len(second)-1]
	if last.Role != domain.RoleTool || last.ToolCallID != "call_now_1" {
		t.Errorf("the tool result was not injected: %+v", last)
	}

	// The execution must be durable, not merely reported.
	if len(h.invocations.executions) != 1 {
		t.Fatalf("expected one recorded execution, got %d", len(h.invocations.executions))
	}
	if h.invocations.executions[0].ToolName != "now" {
		t.Errorf("recorded tool = %q", h.invocations.executions[0].ToolName)
	}
	if len(h.runs.runs) == 0 {
		t.Fatal("the run must be recorded")
	}
	final := h.runs.runs[len(h.runs.runs)-1]
	if final.Status != domain.RunCompleted {
		t.Errorf("final run status = %q, want completed: %+v", final.Status, h.runs.runs)
	}

	// The run row must exist before the first step, because agent_steps has a
	// foreign key to agent_runs. A run that recorded a step first would fail
	// every insert and leave a completed run with no trace at all.
	if h.runs.runs[0].Status != domain.RunRunning {
		t.Errorf("the first write must establish the run as running, got %+v", h.runs.runs[0])
	}
	if h.runs.firstStepAfterFirstRun == false {
		t.Error("a step was recorded before the run row existed; " +
			"it would fail the agent_steps foreign key and be lost")
	}
	if len(h.runs.steps) == 0 {
		t.Error("the step trace must be persisted")
	}
}

func TestGatewayRefusesExternalToolExecution(t *testing.T) {
	// The safety property end to end: an external tool is advertised and the
	// model may call it, but the gateway returns it to the client.
	h := newToolHarness(t, automaticPolicy(), []domain.ToolSpec{externalToolSpec()})
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"tool_execution":{"mode":"automatic"},`+toolDeclaration+`}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if len(h.invocations.executions) != 0 {
		t.Fatal("an external tool must never be executed by the gateway")
	}

	// The model must be told why, so it can continue rather than stall.
	turns := h.provider.seenTurns()
	if len(turns) < 2 {
		t.Fatalf("the run did not continue: %d turns", len(turns))
	}
	last := turns[1][len(turns[1])-1]
	if !strings.Contains(last.Text(), "not executed by the gateway") {
		t.Errorf("the model was not told the call was skipped: %+v", last)
	}
}

func TestStreamingToolExecutionIsRefusedClearly(t *testing.T) {
	// Streamed tool-call frames already reached the client, so a multi-step run
	// is impossible. Saying so beats silently running only the first turn.
	h := newToolHarness(t, automaticPolicy(), []domain.ToolSpec{nowSpec()})
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"stream":true,"tool_execution":{"mode":"automatic"},`+toolDeclaration+`}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, raw)
	}
	if !strings.Contains(string(raw), "stream") {
		t.Errorf("the error must explain the streaming limitation: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// Client-executed tools (gateway execution disabled, the default)
// ---------------------------------------------------------------------------

func TestDisabledGatewayExecutionStreamsAutomaticToolsToClient(t *testing.T) {
	// The agentic-app contract: even an explicit automatic request streams the
	// model's tool calls back untouched instead of failing, so OpenCode and
	// Claude Code can run their own tools.
	h := newClientHarness(t, automaticPolicy(), []domain.ToolSpec{nowSpec()}, "")
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"stream":true,"tool_execution":{"mode":"automatic"},`+toolDeclaration+`}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, raw)
	}
	if strings.Contains(string(raw), "needs stream:false") {
		t.Errorf("an automatic streamed request must not be refused: %s", raw)
	}
	if !strings.Contains(string(raw), "data:") {
		t.Errorf("expected a streamed event body: %s", raw)
	}
	if len(h.invocations.executions) != 0 {
		t.Error("the gateway must not execute tools when gateway execution is disabled")
	}
}

func TestDisabledGatewayExecutionClampsNonStreamingAutomatic(t *testing.T) {
	// Without streaming the clamp is equally in force: the tool call reaches
	// the client with a client_executed report instead of running inside the
	// gateway.
	h := newClientHarness(t, automaticPolicy(), []domain.ToolSpec{nowSpec()}, "")
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"what time is it"}],
		"tool_execution":{"mode":"automatic"},`+toolDeclaration+`}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	var decoded struct {
		Choices []domain.Choice          `json:"choices"`
		Core    *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("the model's tool call must reach the client: %+v", decoded.Choices[0].Message)
	}
	if len(h.invocations.executions) != 0 {
		t.Error("the gateway must not execute tools when gateway execution is disabled")
	}
	// The plain client-executed path carries no gateway run block: there was
	// no run, only a tool call handed back for the client to execute.
	if decoded.Core != nil && decoded.Core.ToolRun != nil &&
		decoded.Core.ToolRun.Status != domain.ToolRunClientExecuted {
		t.Errorf("unexpected tool run status = %q", decoded.Core.ToolRun.Status)
	}
}

func TestDisabledGatewayExecutionDoesNotAdvertiseRegistry(t *testing.T) {
	// A plain request stays plain: registry tools are never injected into a
	// request that declared none, so clients see exactly what they sent.
	h := newClientHarness(t, automaticPolicy(), []domain.ToolSpec{nowSpec()}, "")
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	for i, tools := range h.provider.observedTools() {
		if len(tools) != 0 {
			t.Errorf("turn %d reached the provider with %d undeclared tools", i+1, len(tools))
		}
	}
}

func TestEnabledGatewayExecutionStillAdvertisesRegistry(t *testing.T) {
	// The opt-in path is unchanged: with gateway execution enabled, a
	// tool-less request is still offered the registry's tools.
	h := newToolHarness(t, automaticPolicy(), []domain.ToolSpec{nowSpec()})
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	seen := h.provider.observedTools()
	if len(seen) == 0 || len(seen[0]) == 0 {
		t.Errorf("the opt-in gateway must still advertise registry tools, saw %+v", seen)
	}
}

func TestDisabledPolicyRefusesRequiredToolChoice(t *testing.T) {
	disabled := domain.DefaultToolPolicy()
	disabled.Mode = domain.ToolsDisabled
	h := newToolHarness(t, &disabled, []domain.ToolSpec{nowSpec()})
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"tool_choice":"required",`+toolDeclaration+`}`)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", status, raw)
	}
}

func TestClientCannotExceedPolicyAutonomy(t *testing.T) {
	// A client asking for automatic execution under a manual policy must not
	// get it: autonomy is the operator's decision, not the caller's.
	manual := domain.DefaultToolPolicy()
	manual.Mode = domain.ToolsManual
	h := newToolHarness(t, &manual, []domain.ToolSpec{nowSpec()})
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"tool_execution":{"mode":"automatic"},`+toolDeclaration+`}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if len(h.invocations.executions) != 0 {
		t.Fatal("a client must not be able to opt into gateway execution")
	}
}

func TestDeniedToolIsSkippedAndExplained(t *testing.T) {
	denying := automaticPolicy()
	denying.DeniedTools = []string{"now"}
	h := newToolHarness(t, denying, []domain.ToolSpec{nowSpec()})
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"tool_execution":{"mode":"automatic"},`+toolDeclaration+`}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if len(h.invocations.executions) != 0 {
		t.Fatal("a denied tool must not run")
	}
	turns := h.provider.seenTurns()
	last := turns[1][len(turns[1])-1]
	if !strings.Contains(last.Text(), "denied") {
		t.Errorf("the model should be told the tool is denied: %+v", last)
	}
}

// ---------------------------------------------------------------------------
// Structured output
// ---------------------------------------------------------------------------

func TestStructuredOutputRejectsUnknownType(t *testing.T) {
	h := newToolHarness(t, nil, nil)
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"yaml"}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, raw)
	}
}

func TestStructuredOutputSchemaCheckedOnToolRun(t *testing.T) {
	// The scripted provider answers with prose, so a strict json_schema contract
	// must fail the run rather than return unparseable content.
	h := newToolHarness(t, automaticPolicy(), []domain.ToolSpec{nowSpec()})
	status, raw := h.post(t,
		`{"model":"`+testModel+`","messages":[{"role":"user","content":"hi"}],
		"tool_execution":{"mode":"automatic"},
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,
		"schema":{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}}},
		`+toolDeclaration+`}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a schema violation: %s", status, raw)
	}
	if !strings.Contains(string(raw), "schema") {
		t.Errorf("the error must mention the schema: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// Structured output without tools
// ---------------------------------------------------------------------------

func TestPlainChatHonoursJSONObject(t *testing.T) {
	// A request with no tools and no execution block is the ordinary path, and
	// it must still honour response_format: the provider's valid JSON is
	// returned untouched and the conformance report is attached.
	h := newToolHarnessAnswering(t, nil, nil, `{"ok":true,"note":"all good"}`)
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"status?"}],
		"response_format":{"type":"json_object"}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	var decoded struct {
		Choices []domain.Choice          `json:"choices"`
		Core    *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := decoded.Choices[0].Message.Text(); !strings.Contains(got, `"ok":true`) {
		t.Errorf("the answer should survive unchanged: %q", got)
	}
	if decoded.Core == nil || decoded.Core.Structured == nil {
		t.Fatal("structured conformance must be reported")
	}
	if decoded.Core.Structured.Requested != domain.FormatJSONObject {
		t.Errorf("requested = %q", decoded.Core.Structured.Requested)
	}
	if decoded.Core.Structured.Valid == nil || !*decoded.Core.Structured.Valid {
		t.Errorf("valid = %v (%s)", decoded.Core.Structured.Valid, decoded.Core.Structured.Error)
	}
	if decoded.Core.ToolRun != nil {
		t.Error("a request with no tools must not report a tool run")
	}
}

func TestPlainChatRepairsFencedJSON(t *testing.T) {
	// Models routinely wrap JSON in a code fence even when told not to. The
	// gateway extracts it and says so, rather than handing back unusable text.
	fenced := "```json\n{\"ok\":true}\n```"
	h := newToolHarnessAnswering(t, nil, nil, fenced)
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"status?"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":
		{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}}}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if !strings.Contains(string(raw), `"repaired":true`) {
		t.Errorf("the repair must be reported: %s", raw)
	}
}

func TestPlainChatRejectsSchemaViolation(t *testing.T) {
	// A strict schema is a contract. Prose is not a valid answer to it, so the
	// request fails instead of returning content the caller cannot parse.
	h := newToolHarnessAnswering(t, nil, nil, "I could not find anything.")
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"status?"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"s","strict":true,"schema":
		{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, raw)
	}
	if !strings.Contains(string(raw), "JSON schema") {
		t.Errorf("the error must name the schema violation: %s", raw)
	}
}

func TestPlainChatRejectsNonObjectJSONObjectMode(t *testing.T) {
	// json_object promises an object. A bare array is not one, and a caller
	// that got it would fail at its own unmarshal with no useful message.
	h := newToolHarnessAnswering(t, nil, nil, "[1, 2, 3]")
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"status?"}],
		"response_format":{"type":"json_object"}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, raw)
	}
}

func TestPlainChatRejectsUnsupportedSchemaKeywords(t *testing.T) {
	// An unsupported keyword is rejected rather than ignored, so a caller never
	// believes a constraint was enforced when it was silently dropped.
	h := newToolHarnessAnswering(t, nil, nil, `{"ok":true}`)
	status, raw := h.post(t, `{"model":"`+testModel+`","messages":[{"role":"user","content":"status?"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":
		{"type":"object","if":{"required":["ok"]},"properties":{"ok":{"type":"boolean"}}}}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, raw)
	}
}
