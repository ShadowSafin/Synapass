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
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
	"github.com/shadowsafin/synapass/internal/routing"
	"github.com/shadowsafin/synapass/internal/telemetry"
	"github.com/shadowsafin/synapass/internal/tokens"
	"github.com/shadowsafin/synapass/internal/tools"
)

// Header names a client can use to express routing intent.
//
// These are namespaced rather than standard because they are Synapass's own
// extension: a client that does not send them gets the policy's defaults, which
// keeps the gateway drop-in compatible with an OpenAI SDK.
const (
	headerMaxCostUSD      = "X-Synapass-Max-Cost-USD"
	headerLatencyTargetMS = "X-Synapass-Latency-Target-Ms"
	headerPolicyID        = "X-Synapass-Policy"
	headerNoFallback      = "X-Synapass-No-Fallback"
	headerEndpointID      = "X-Synapass-Endpoint"
	headerRegion          = "X-Synapass-Region"
	headerSensitivity     = "X-Synapass-Sensitivity"
	headerBatch           = "X-Synapass-Batch"
	headerNoCache         = "X-Synapass-No-Cache"
)

// headerThinking signals that a non-streaming response carries a thinking
// trace (reasoning_content on a message). Streams cannot carry it: headers
// are committed before the first chunk arrives, while thinking arrives
// mid-stream. Stream clients detect thinking by the presence of
// delta.reasoning_content frames instead.
const headerThinking = "X-Synapass-Thinking"

// responseHasReasoning reports whether any choice carries a thinking trace.
func responseHasReasoning(resp domain.ChatCompletionResponse) bool {
	for _, ch := range resp.Choices {
		if ch.Message != nil && ch.Message.Reasoning != "" {
			return true
		}
	}
	return false
}

// handleChatCompletions serves POST /v1/chat/completions.
//
// Phase 2 request flow (every step is traced):
//
//  1. authenticate (middleware)  2. load tenant/endpoint policy
//  3. classify task              4. compute shaping plan
//  5. check cache                6. evaluate health/scores
//  7. select route               8. execute provider call
//  9. fall back if needed        10. persist traces/metrics/scores/audit
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rc := requestContext(ctx)
	if rc == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "request context is missing"), nil)
		return
	}
	rc.Labels["method"] = r.Method
	rc.Labels["path"] = r.URL.Path

	if s.metrics != nil {
		s.metrics.IncInFlight(true)
		defer s.metrics.DecInFlight(true)
	}

	// started anchors elapsed latency for every outcome below, including the
	// early failures. A rejected request still consumed gateway time and still
	// gets a usage row and a request log: without this, requests that fail
	// before reaching a provider are invisible to the dashboard and to billing
	// reconciliation, which is exactly the traffic an operator debugs first.
	started := time.Now()

	// fail writes an error response and records the outcome, so pre-provider
	// failures (validation, policy deny, rate limit, no eligible provider) are
	// persisted and metered exactly like provider failures.
	fail := func(err error) {
		writeError(w, err, publicMeta(r, rc, nil))
		bctx, cancel := bookkeepingContext()
		defer cancel()
		s.recordOutcome(bctx, rc, rc.Resolution, nil,
			domain.TokenUsage{PromptTokens: rc.PromptTokens, Estimated: true}.Normalize(),
			domain.Cost{}, time.Since(started),
			domain.AsError(err).HTTPStatus(), err)
	}

	var body domain.ChatCompletionRequest
	// The inference surface decodes leniently: OpenAI-compatible clients
	// (VS Code Copilot Chat, Cursor, SDKs) send extension fields such as
	// cache_control, store or service_tier that Synapass accepts and
	// ignores. Rejecting them with DisallowUnknownFields turns every new
	// client feature into a 400. Strict decoding stays on the admin
	// surface, where a typo must fail loudly.
	if err := decodeLenient(r, s.config.HTTP.MaxBodyBytes, &body); err != nil {
		fail(err)
		return
	}
	// Shorthands (prompt/system) expand to messages first, so validation and
	// everything downstream sees exactly one input shape.
	if err := body.Normalize(); err != nil {
		fail(err)
		return
	}
	// Phase 4: tool declarations and the execution block are checked before
	// routing, so a malformed tool is a 400 with a field name rather than an
	// opaque upstream rejection several hundred milliseconds later.
	if err := body.ToolExecution.Validate(); err != nil {
		fail(err)
		return
	}
	if _, err := domain.ValidateTools(body.Tools); err != nil {
		fail(err)
		return
	}
	if err := body.Validate(); err != nil {
		fail(err)
		return
	}

	// Populate the routing inputs from the parsed request. All of this is derived
	// here rather than inside the engine because it is a property of the wire
	// format, not of routing.
	rc.RequestedModel = body.Model
	rc.Stream = body.Streaming()
	rc.MaxOutputTokens = body.RequestedMaxTokens()
	rc.RequestedOutputTokens = rc.MaxOutputTokens
	rc.N = 1
	if body.N != nil {
		rc.N = *body.N
	}
	rc.PromptTokens = tokens.EstimateRequest(&body)
	rc.RequiredCapabilities = body.RequiredCapabilities()
	rc.Messages = body.Messages
	rc.Labels["stream"] = strconv.FormatBool(rc.Stream)
	// Request size for size guards.
	if r.ContentLength > 0 {
		rc.RequestBytes = int(r.ContentLength)
	}
	applyRoutingIntent(r, rc)
	applyPhase2Intent(r, rc)

	// Guardrail: tenant emergency override denies before any work.
	if s.guard != nil {
		if err := s.guard.CheckTenant(rc.TenantID()); err != nil {
			if s.metrics != nil {
				s.metrics.ObserveGuardrailBlock("tenant_override")
			}
			fail(err)
			return
		}
	}

	// Resolve the policy before routing so rate limits and budgets are enforced
	// before any provider work happens. The engine resolves it again, which is a
	// cache hit, so the cost is a map lookup rather than a second query.
	policy, err := s.policies.Resolve(ctx, rc)
	if err != nil {
		fail(err)
		return
	}

	// 3. Classify request type (rules-first, never blocks routing).
	if s.classifier != nil {
		rc.Task = s.classifier.Classify(&body, rc.PromptTokens)
		if s.metrics != nil {
			s.metrics.ObserveClassification(string(rc.Task.PrimaryOrDefault()))
		}
	} else {
		rc.Task = domain.TaskClassification{Task: domain.TaskChat, Confidence: 0.5, Signals: []string{"classifier_disabled"}, Source: "none"}
	}

	// 2b. Fine-grained policy evaluation produces the decision the router consumes.
	if s.policyEng != nil {
		dec := s.policyEng.Evaluate(ctx, policy, rc)
		rc.PolicyDecision = dec
		if !dec.Allowed {
			if s.metrics != nil {
				s.metrics.ObserveGuardrailBlock("policy_deny")
			}
			denyErr := domain.NewError(domain.ErrCodePermission, dec.DenyMessage)
			denyErr.Status = http.StatusForbidden
			fail(denyErr)
			return
		}
		// Policy-level hard caps tighten the ceiling.
		if s.guard != nil && rc.TenantID() != "" {
			if cap, ok := s.guard.HardCap(rc.TenantID()); ok && cap > 0 {
				if rc.CostCeilingUSD <= 0 || cap < rc.CostCeilingUSD {
					rc.CostCeilingUSD = cap
				}
			}
		}
	}

	// 4. Prompt shaping plan (visible in traces).
	shapedBody := &body
	var shapeRecord domain.PromptShape
	if s.shaper != nil {
		plan := s.shaper.PlanFor(rc.Task, policy, rc.PolicyDecision)
		rc.ShapePlan = plan
		shaped, rec := s.shaper.Apply(&body, plan)
		shapedBody = shaped
		shapeRecord = rec
		shapeRecord.RequestID = rc.RequestID
		if rc.TenantID() != "" {
			shapeRecord.TenantID = rc.TenantID()
		}
		_ = shapeRecord
	} else if rc.PolicyDecision != nil {
		rc.ShapePlan = rc.PolicyDecision.Shaping
	}

	if err := s.enforcePreflight(ctx, rc, policy); err != nil {
		fail(err)
		return
	}

	// Endpoint scopes constrain routing beyond the stored policy. The scope
	// row is loaded here so the engine can fold its override into this
	// request's policy copy; an unknown or disabled slug fails fast with a
	// message that names the slug, because a client sending a scope that does
	// nothing is always a configuration bug worth surfacing.
	if err := s.applyEndpointScope(ctx, rc); err != nil {
		fail(err)
		return
	}

	// Phase 4: resolve the registry snapshot, the effective tool policy and the
	// structured-output contract before anything is cached or routed.
	toolCtx, err := s.resolveToolContext(ctx, &body, rc)
	if err != nil {
		fail(err)
		return
	}

	// 5. Cache check (Phase 5: policy-aware, tenant-isolated, full-key).
	//
	// The flow is authenticate → tenant/endpoint policy → cache decision →
	// exact → prefix (short only) → semantic. A bypass names its reason so
	// the miss is explainable in metrics, traces and the dashboard.
	if s.cache != nil && s.cache.Enabled() {
		lookup, decision := s.cacheLookup(ctx, rc, &body, policy, toolCtx)
		rc.CacheDecision = decision
		if lookup != nil && lookup.Hit {
			s.serveCacheHit(w, rc, policy, *lookup, shapedBody, debugRequested(r))
			return
		}
	} else {
		// No cache to consult: name the bypass so metrics, traces and
		// the dashboard record a bypass rather than a miss. A miss
		// means "looked and found nothing"; this looked at nothing.
		rc.CacheDecision = &domain.CacheDecision{Cacheable: false, BypassReason: domain.CacheBypassDisabled}
	}

	decision, err := s.engine.Resolve(ctx, rc)
	if err != nil {
		fail(err)
		return
	}

	// Guardrail: kill-switched providers must not be attempted even if the
	// engine selected them before the switch flipped.
	if s.guard != nil {
		if err := s.guard.CheckProvider(decision.Chosen.ProviderID, decision.Chosen.ProviderName); err != nil {
			if s.metrics != nil {
				s.metrics.ObserveGuardrailBlock("kill_switch")
			}
			fail(err)
			return
		}
		// Fallback blocking per tenant/policy scope.
		if ok, reason := s.guard.FallbackBlocked(decision.PolicyID); ok {
			decision.Chain = decision.Chain.Truncate(1)
			decision.Fallback.Enabled = false
			decision.Reason += "; fallback blocked: " + reason
		} else if ok, reason := s.guard.FallbackBlocked(rc.TenantID()); ok {
			decision.Chain = decision.Chain.Truncate(1)
			decision.Fallback.Enabled = false
			decision.Reason += "; fallback blocked: " + reason
		}
	}

	// A client-supplied "no fallback" header is honoured by truncating the chain to
	// one target. It exists for callers that must know exactly which provider served
	// them, such as a compliance-sensitive workload.
	if noFallbackRequested(r) {
		decision.Chain = decision.Chain.Truncate(1)
		decision.Reason += "; fallback disabled by request header"
	}

	// A request that declared no tools but has registry tools available is
	// offered the registry's, so the agent workflow works without the client
	// having to know what is installed. Request tools always win. Advertisement
	// only happens when gateway execution is enabled: otherwise a plain chat
	// request would gain tools the client never asked for.
	if toolCtx != nil && s.gatewayExecution() && len(shapedBody.Tools) == 0 {
		if advertised := toolCtx.Registry.Advertise(); len(advertised) > 0 {
			shapedBody.Tools = advertised
			if len(shapedBody.ToolChoice) == 0 {
				shapedBody.ToolChoice = toolChoiceJSON(toolCtx.Choice)
			}
		}
	}

	req := &providers.Request{
		Model:        decision.Chosen.Model,
		Params:       shapedBody,
		Ref:          decision.Chosen.Ref(),
		MaxTokens:    rc.MaxOutputTokens,
		PromptTokens: rc.PromptTokens,
		Stream:       rc.Stream,
	}

	var sse *sseWriter
	if rc.Stream {
		sse, err = newSSEWriter(w)
		if err != nil {
			fail(err)
			return
		}
	}

	// Phase 4: gateway-side tool execution. Only automatic mode takes this
	// path, and only for a non-streaming request: streamed tool-call frames
	// have already reached the client by the time a tool result exists, so a
	// multi-step run cannot be un-sent. That limitation is reported rather than
	// silently ignored.
	if toolCtx != nil && s.gatewayExecution() && toolCtx.automatic() && len(shapedBody.Tools) > 0 {
		if rc.Stream {
			fail(domain.NewError(domain.ErrCodeInvalidRequest,
				"gateway-side tool execution needs stream:false; "+
					"stream the model's tool calls and execute them client-side, "+
					"or set tool_execution.mode to manual"))
			return
		}
		s.runToolCompletion(ctx, w, rc, decision, toolCtx, shapedBody, started, debugRequested(r))
		return
	}

	if err := s.runCompletion(ctx, w, rc, decision, req, *shapedBody, sse, started, debugRequested(r)); err != nil {
		s.respondError(w, sse, rc, err, started, debugRequested(r))
		return
	}
}

// toolChoiceJSON renders the resolved tool_choice for a request that did not
// carry one, so an agent run advertises the registry's intent to the provider.
func toolChoiceJSON(choice domain.ToolChoice) json.RawMessage {
	switch choice.Mode {
	case domain.ToolChoiceNone:
		return json.RawMessage(`"none"`)
	case domain.ToolChoiceRequired:
		return json.RawMessage(`"required"`)
	case domain.ToolChoiceNamed:
		if choice.Name == "" {
			return nil
		}
		encoded, err := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": choice.Name},
		})
		if err != nil {
			return nil
		}
		return encoded
	default:
		return nil
	}
}

// runCompletion executes the request and writes the response.
//
// A returned error may have been produced either before or after the response was
// committed; the caller distinguishes the two cases from the writer state rather
// than from the error, because the error itself carries no such distinction.
func (s *Server) runCompletion(
	ctx context.Context,
	w http.ResponseWriter,
	rc *domain.RequestContext,
	decision *domain.RouteDecision,
	req *providers.Request,
	body domain.ChatCompletionRequest,
	sse *sseWriter,
	started time.Time,
	debug bool,
) error {
	var handler providers.StreamHandler
	if sse != nil {
		handler = s.streamHandler(sse, rc, decision, debug)
	}

	result, err := s.executor.Execute(ctx, rc, req, handler)
	if err != nil {
		return err
	}

	elapsed := time.Since(started)
	usage := result.Response.Usage.Normalize()
	if usage.Estimated && usage.TotalTokens == 0 {
		// No provider-reported usage and nothing to estimate from: fall back to the
		// prompt estimate plus the assembled completion, so the record is never zero.
		usage = domain.TokenUsage{
			PromptTokens:     rc.PromptTokens,
			CompletionTokens: tokens.EstimateText(result.Response.Content()),
			Estimated:        true,
		}.Normalize()
	}

	cost := costForUsage(decision.Chosen, usage)

	// Completion bounding is computed for both paths so a shortened answer is
	// always reported, never silently delivered.
	completion := completionMeta(rc, decision, result.Response, usage)

	if sse != nil {
		// The terminal usage frame is only sent when the client asked for it, which
		// mirrors OpenAI's stream_options behaviour and avoids surprising clients
		// with an extra frame.
		if body.StreamOptions != nil && body.StreamOptions.IncludeUsage {
			if err := sse.WriteEvent(s.usageChunk(result.Response, rc, decision, result, usage, cost, debug, completion)); err != nil {
				return err
			}
		}
		if err := sse.WriteDone(); err != nil {
			return err
		}
		logTruncation(s.logger, s.metrics, rc, decision, completion, "")
		// A cleanly completed stream is a complete response like any
		// other: store it when the request's cache decision allows it.
		// Failed and client-aborted streams return through respondError,
		// never through here, so the cache cannot learn them.
		s.storeCompletedStream(ctx, rc, decision, body, result, usage, cost, elapsed, debug)
	} else {
		response := s.buildResponse(result.Response, rc, decision, result, usage, cost, elapsed, debug)
		// A requested response_format is a contract, checked here rather than
		// assumed. This mirrors the tool path so both answer the same way: a
		// json_schema the answer violates is a 400, because returning content
		// the caller cannot parse is worse than telling it why.
		if rc.StructuredOutput.Mode != domain.FormatNone {
			structured := validateStructuredOutput(rc.StructuredOutput, result.Response.Content(), false)
			if structured != nil && structured.Valid != nil && !*structured.Valid {
				return domain.Errorf(domain.ErrCodeInvalidRequest,
					"the response did not match the requested JSON schema: %s", structured.Error)
			}
			if response.Synapass != nil {
				response.Synapass.Structured = structured
			}
		}
		if response.Synapass != nil {
			response.Synapass.Completion = completion
		}
		if responseHasReasoning(response) {
			w.Header().Set(headerThinking, "present")
		}
		writeJSON(w, http.StatusOK, response)
		logTruncation(s.logger, s.metrics, rc, decision, completion, "")
		// Store for future hits when the request's cache decision allows it.
		// Scopes are namespaced in the key, so scoped traffic caches safely
		// per scope rather than bypassing entirely.
		if payload, merr := jsonMarshal(response); merr == nil {
			servedBy, servedModel, _, _ := servingTarget(decision, result)
			policyID, policyVersion := "", 0
			if decision != nil {
				policyID = decision.PolicyID
			}
			if rc.PolicyDecision != nil && rc.PolicyDecision.PolicyID != "" {
				policyID = rc.PolicyDecision.PolicyID
				policyVersion = rc.PolicyDecision.PolicyVersion
			}
			if servedModel == "" {
				servedModel = body.Model
			}
			s.cacheStore(ctx, rc, &body, payload, servedBy, servedModel,
				policyID, policyVersion, usage, cost.USD, result.ProviderLatencyMS)
		}
	}

	s.recordOutcome(ctx, rc, decision, result, usage, cost, elapsed, http.StatusOK, nil)
	return nil
}

// serveCacheHit returns a cached response without calling a provider.
func (s *Server) serveCacheHit(w http.ResponseWriter, rc *domain.RequestContext, policy *domain.RoutingPolicy, lookup domain.CacheLookupResult, body *domain.ChatCompletionRequest, debug bool) {
	started := rc.ReceivedAt
	if started.IsZero() {
		started = time.Now()
	}
	// Cached body is a full ChatCompletionResponse.
	var cached domain.ChatCompletionResponse
	if err := json.Unmarshal(lookup.Body, &cached); err != nil {
		// Corrupt cache entry: it fails closed by serving the stored
		// bytes verbatim with cache metadata rather than failing the
		// request, and it is logged so the corruption is visible. The
		// entry expires via its TTL; it is never re-stored.
		if s.logger != nil {
			s.logger.Warn("serving corrupt cache entry verbatim",
				"request_id", rc.RequestID.String(),
				"cache_key", lookup.Key,
				"cache_kind", string(lookup.Kind),
			)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Synapass-Cache", string(lookup.Kind))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(lookup.Body)
		bctx, cancel := bookkeepingContext()
		defer cancel()
		s.recordCacheOutcome(bctx, rc, policy, lookup, time.Since(started))
		return
	}
	// Refresh request-scoped metadata.
	cached.Synapass = publicMetaFrom(rc, func(m *domain.ResponseMetadata) {
		m.CacheHit = true
		m.CacheKind = string(lookup.Kind)
		m.LatencyMS = time.Since(started).Milliseconds()
		m.Task = string(rc.Task.Task)
		m.Attempts = 0
		m.CacheSimilarity = lookup.Similarity
		m.CacheReuseCount = lookup.ReuseCount
	}, debug)
	if rc.Stream {
		// A streaming client asked for SSE, so the stored answer replays
		// as SSE: one content frame carrying the routing metadata, one
		// terminal frame, the usage frame when asked for, then [DONE].
		// The bytes differ from a live stream; the content does not.
		s.serveCacheHitStream(w, rc, cached, lookup, body, debug)
		return
	}
	if responseHasReasoning(cached) {
		w.Header().Set(headerThinking, "present")
	}
	writeJSON(w, http.StatusOK, cached)
	bctx, cancel := bookkeepingContext()
	defer cancel()
	s.recordCacheOutcome(bctx, rc, policy, lookup, time.Since(started))
	// Prometheus cache counters are emitted once by the recorder inside
	// recordCacheOutcome; emitting them here too would double-count hits.
	// Bump the durable reuse counter best-effort.
	if s.repos != nil && s.repos.CacheEntries != nil && lookup.Key != "" {
		s.repos.CacheEntries.BumpHit(bctx, cacheEntryKey(rc.TenantID(), lookup.Key))
	}
}

// cacheEntryKey maps a lookup key back to the entry key stored by
// UpsertMeta. Lookups carry the tenant-namespaced form
// ("tenant:<id>:exact:<hash>"); the metadata row is keyed by the bare form
// ("exact:<hash>"), so the namespace must be stripped or the bump — and
// with it the dashboard top-prompts ranking — silently misses every row.
func cacheEntryKey(tenantID, lookupKey string) string {
	if tenantID != "" {
		if bare := strings.TrimPrefix(lookupKey, "tenant:"+tenantID+":"); bare != lookupKey {
			return bare
		}
	}
	return lookupKey
}

// serveCacheHitStream replays a cached response to a streaming client.
//
// The stored body is a full ChatCompletionResponse; the replay renders it
// as the same SSE shape a live stream would have produced: a content frame
// carrying the routing metadata, a terminal frame, the usage frame when
// asked for, then [DONE]. A failure to establish the stream fails the
// request rather than silently downgrading it to JSON, because the client
// is parsing events.
func (s *Server) serveCacheHitStream(w http.ResponseWriter, rc *domain.RequestContext, cached domain.ChatCompletionResponse, lookup domain.CacheLookupResult, body *domain.ChatCompletionRequest, debug bool) {
	started := rc.ReceivedAt
	if started.IsZero() {
		started = time.Now()
	}
	w.Header().Set("X-Synapass-Cache", string(lookup.Kind))
	sse, err := newSSEWriter(w)
	if err != nil {
		writeError(w, err, publicMetaFrom(rc, nil, debug))
		bctx, cancel := bookkeepingContext()
		defer cancel()
		s.recordCacheOutcome(bctx, rc, nil, lookup, time.Since(started))
		return
	}
	content := ""
	reasoning := ""
	if len(cached.Choices) > 0 && cached.Choices[0].Message != nil {
		content = cached.Choices[0].Message.Content.PlainText()
		reasoning = cached.Choices[0].Message.Reasoning
	}
	model := firstNonEmptyString(cached.Model, rc.RequestedModel)
	created := cached.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	meta := publicMetaFrom(rc, func(m *domain.ResponseMetadata) {
		m.CacheHit = true
		m.CacheKind = string(lookup.Kind)
		m.RoutedModel = model
		m.RequestedModel = rc.RequestedModel
		m.Task = string(rc.Task.Task)
		m.Attempts = 0
		m.CacheSimilarity = lookup.Similarity
		m.CacheReuseCount = lookup.ReuseCount
	}, debug)
	delta := domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(content)}
	stop := domain.FinishStop
	frames := []domain.ChatCompletionChunk{}
	// A cached trace replays ahead of the answer, mirroring live-stream
	// order where thinking arrives before content.
	if reasoning != "" {
		thinking := domain.ChatMessage{Role: domain.RoleAssistant, Reasoning: reasoning}
		frames = append(frames, domain.ChatCompletionChunk{
			ID: firstNonEmptyString(cached.ID, rc.RequestID.String()),
			Object: domain.ObjectChatCompletionChunk,
			Created: created,
			Model: model,
			SystemFingerprint: cached.SystemFingerprint,
			Choices: []domain.Choice{{Index: 0, Delta: &thinking}},
			Synapass: meta,
		})
		meta = nil
	}
	frames = append(frames,
		domain.ChatCompletionChunk{
			ID: firstNonEmptyString(cached.ID, rc.RequestID.String()),
			Object: domain.ObjectChatCompletionChunk,
			Created: created,
			Model: model,
			SystemFingerprint: cached.SystemFingerprint,
			Choices: []domain.Choice{{Index: 0, Delta: &delta}},
			Synapass: meta,
		},
		domain.ChatCompletionChunk{
			ID: firstNonEmptyString(cached.ID, rc.RequestID.String()),
			Object: domain.ObjectChatCompletionChunk,
			Created: created,
			Model: model,
			Choices: []domain.Choice{{Index: 0, FinishReason: &stop}},
		},
	)
	for _, frame := range frames {
		if werr := sse.WriteEvent(frame); werr != nil {
			// Client went away mid-replay: the hit was still served from
			// cache, so it is recorded, not turned into an error.
			break
		}
	}
	if body != nil && body.StreamOptions != nil && body.StreamOptions.IncludeUsage && cached.Usage != nil {
		usageCopy := *cached.Usage
		if werr := sse.WriteEvent(domain.ChatCompletionChunk{
			ID: firstNonEmptyString(cached.ID, rc.RequestID.String()),
			Object: domain.ObjectChatCompletionChunk,
			Created: created,
			Model: model,
			Choices: []domain.Choice{},
			Usage: &usageCopy,
		}); werr != nil {
			_ = werr
		}
	}
	_ = sse.WriteDone()
	bctx, cancel := bookkeepingContext()
	defer cancel()
	s.recordCacheOutcome(bctx, rc, nil, lookup, time.Since(started))
	if s.repos != nil && s.repos.CacheEntries != nil && lookup.Key != "" {
		s.repos.CacheEntries.BumpHit(bctx, cacheEntryKey(rc.TenantID(), lookup.Key))
	}
}

// recordCacheOutcome records a cache hit as a usage/log entry without provider cost.
func (s *Server) recordCacheOutcome(ctx context.Context, rc *domain.RequestContext, policy *domain.RoutingPolicy, lookup domain.CacheLookupResult, elapsed time.Duration) {
	if s.recorder == nil || rc == nil {
		return
	}
	policyID := ""
	if policy != nil {
		policyID = policy.ID
	}
	usage := domain.TokenUsage{PromptTokens: rc.PromptTokens, Estimated: true}.Normalize()
	if lookup.Meta != nil && lookup.Meta.Usage.TotalTokens > 0 {
		usage = lookup.Meta.Usage
	}
	// A cache serve bills nothing, but the estimate records what serving it
	// would have cost — that standing delta is the measured cache saving.
	var estimate domain.Cost
	if rc.Resolution != nil {
		estimate = rc.Resolution.EstimatedCost
	}
	out := telemetry.RequestOutcome{
		RequestID:      rc.RequestID,
		TraceID:        rc.TraceID,
		TenantID:       rc.TenantID(),
		APIKeyID:       rc.APIKeyID(),
		Provider:       "cache",
		Model:          rc.RequestedModel,
		RequestedModel: rc.RequestedModel,
		PolicyID:       policyID,
		RequestType:    rc.RequestType,
		Usage:          usage,
		Cost:           domain.Cost{},
		EstimateCost:   estimate,
		EndpointID:     rc.EndpointID,
		LatencyMS:      elapsed.Milliseconds(),
		CacheHit:       true,
		CacheKind:      string(lookup.Kind),
		CacheLookupMS:  lookup.LookupMS,
		CacheSimilarity: lookup.Similarity,
		CacheReuseCount: lookup.ReuseCount,
		CacheLatencySavedMS: lookup.LatencySavedMS,
		Streaming:      rc.Stream,
		Status:         http.StatusOK,
		Outcome:        domain.OutcomeSuccess,
		ClientIP:       rc.ClientIP,
		UserAgent:      rc.UserAgent,
		Task:            rc.Task,
		Shaping:         rc.ShapePlan,
		PolicyDecision:  rc.PolicyDecision,
		Messages:        rc.Messages,
		MaxOutputTokens: rc.MaxOutputTokens,
	}
	s.recorder.RecordRequest(ctx, out)
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// completionMeta builds the response block that explains how the answer was
// bounded and whether it finished.
//
// It returns nil when nothing was capped and the model stopped on its own
// terms, so an ordinary short response gains no new keys. Everything it reports
// is derived from the request, the decision and the provider's own finish
// reason: the point is that a caller can never be left guessing whether a
// short answer was finished or cut.
func completionMeta(
	rc *domain.RequestContext,
	decision *domain.RouteDecision,
	resp *providers.Response,
	usage domain.TokenUsage,
) *domain.CompletionMeta {
	if rc == nil {
		return nil
	}
	applied := effectiveAppliedTokens(decision, rc)

	meta := &domain.CompletionMeta{
		RequestedTokens: requestedOutputTokens(rc),
		AppliedTokens:   applied,
		PromptTokens:    usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
	}
	if decision != nil {
		meta.BudgetMS = decision.Timeout.PerAttempt.Milliseconds()
	}

	// The provider's own reason is authoritative: `length` means the model was
	// still generating when the applied ceiling was reached.
	if finish := responseFinishReason(resp); finish != "" {
		meta.FinishReason = string(finish)
		if finish == domain.FinishLength {
			meta.Truncated = true
			meta.Reason = "max_tokens"
		}
	}
	// Nothing was capped and the model finished: a complete answer must look
	// exactly as it did before this block existed.
	if !meta.Truncated && meta.AppliedTokens == 0 {
		return nil
	}
	return meta
}

// responseFinishReason extracts the finish reason from a completion.
func responseFinishReason(resp *providers.Response) domain.FinishReason {
	if resp == nil {
		return ""
	}
	for _, choice := range resp.Choices {
		if choice.FinishReason != nil {
			return *choice.FinishReason
		}
	}
	return ""
}

// requestedOutputTokens returns what the client asked for, or 0.
func requestedOutputTokens(rc *domain.RequestContext) int {
	if rc == nil || rc.Resolution == nil {
		return 0
	}
	// The decision does not record the raw client value, so it is recovered
	// from the effective limit only when it was clamped: the router sets
	// MaxOutputTokens to the policy default when the client said nothing, which
	// is indistinguishable from an explicit request of the same size.
	return rc.RequestedOutputTokens
}

// effectiveAppliedTokens is the ceiling actually sent to the provider.
//
// It mirrors routing.effectiveMaxTokens: the client's own request, narrowed by a
// per-target override, and zero when the client asked for nothing. Reporting the
// policy ceiling here would be a lie — the provider never saw it — and would make
// every answer look capped when the client had set no limit.
func effectiveAppliedTokens(decision *domain.RouteDecision, rc *domain.RequestContext) int {
	if decision == nil || rc == nil || rc.RequestedOutputTokens <= 0 {
		return 0
	}
	maxTokens := rc.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = rc.RequestedOutputTokens
	}
	if target := decision.Chosen; target.MaxOutputTokens > 0 &&
		(maxTokens <= 0 || target.MaxOutputTokens < maxTokens) {
		maxTokens = target.MaxOutputTokens
	}
	return maxTokens
}

// truncatedUsage is the usage reported when a stream was cut short by a
// deadline: the tokens the provider did emit are kept, because they were
// really generated and billed.
func logTruncation(
	logger *slog.Logger,
	metrics *telemetry.Metrics,
	rc *domain.RequestContext,
	decision *domain.RouteDecision,
	meta *domain.CompletionMeta,
	reason string,
) {
	if meta == nil || !meta.Truncated {
		return
	}
	tenant, provider, model := "", "", ""
	if rc != nil {
		tenant = rc.TenantID()
	}
	if decision != nil {
		provider = decision.Chosen.ProviderName
		model = decision.Chosen.Model
	}
	if logger != nil {
		logger.Warn("response was truncated by a limit",
			"request_id", requestIDOf(rc),
			"reason", firstNonEmptyStr(reason, meta.Reason),
			"finish_reason", meta.FinishReason,
			"prompt_tokens", meta.PromptTokens,
			"completion_tokens", meta.CompletionTokens,
			"applied_max_tokens", meta.AppliedTokens,
			"budget_ms", meta.BudgetMS,
			"provider", provider,
			"model", model,
		)
	}
	if metrics != nil {
		metrics.ObserveTruncation(tenant, provider, firstNonEmptyStr(reason, meta.Reason))
	}
}

func requestIDOf(rc *domain.RequestContext) string {
	if rc == nil {
		return ""
	}
	return rc.RequestID.String()
}

// truncationOnStream builds the completion block for a stream that a deadline
// cut short. The bytes already sent cannot be recalled, so the block exists to
// make the incompleteness explicit to the client, the metric and the log.
func (s *Server) truncationOnStream(rc *domain.RequestContext, decision *domain.RouteDecision, err *domain.Error) *domain.CompletionMeta {
	meta := &domain.CompletionMeta{
		Truncated: true,
		Reason:    "timeout",
		TimedOut:  true,
		PromptTokens: rc.PromptTokens,
	}
	if rc != nil {
		meta.RequestedTokens = rc.RequestedOutputTokens
	}
	if decision != nil {
		meta.AppliedTokens = effectiveAppliedTokens(decision, rc)
		meta.BudgetMS = decision.Timeout.PerAttempt.Milliseconds()
	}
	if err != nil {
		meta.FinishReason = string(domain.FinishStop) + "_interrupted"
	}
	return meta
}

// observeTruncation records a truncation on the metric, the ratio histogram and
// the structured log.
func (s *Server) observeTruncation(rc *domain.RequestContext, decision *domain.RouteDecision, meta *domain.CompletionMeta, reason string) {
	if meta == nil || !meta.Truncated {
		return
	}
	provider := ""
	if decision != nil {
		provider = decision.Chosen.ProviderName
	}
	logTruncation(s.logger, s.metrics, rc, decision, meta, reason)
	if s.metrics != nil && meta.AppliedTokens > 0 {
		tenant := ""
		if rc != nil {
			tenant = rc.TenantID()
		}
		s.metrics.ObserveCompletionRatio(tenant, provider, meta.CompletionTokens, meta.AppliedTokens)
	}
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// servingTarget reports who actually answered a request.
//
// The decision records where the request was sent first; the trace records what
// happened. After a failover those differ, so the trace's last successful attempt
// is the only honest source of provider attribution. Falling back to the chosen
// target keeps the metadata populated for the paths that never reached a provider.
func servingTarget(decision *domain.RouteDecision, result *routing.ExecuteResult) (provider, model string, fallbackUsed bool, attempts int) {
	if decision != nil {
		provider = decision.Chosen.ProviderName
		model = decision.Chosen.Model
	}
	if result == nil {
		return provider, model, false, 0
	}

	attempts = result.Attempts
	fallbackUsed = result.FallbackUsed

	if trace := result.Trace; trace != nil {
		for i := len(trace.Attempts) - 1; i >= 0; i-- {
			attempt := trace.Attempts[i]
			if attempt.ErrorCode != "" {
				continue
			}
			if attempt.Target.ProviderName != "" {
				provider = attempt.Target.ProviderName
			}
			if attempt.Target.Model != "" {
				model = attempt.Target.Model
			}
			break
		}
	}
	return provider, model, fallbackUsed, attempts
}

// streamResponseStorable reports whether an assembled stream response may
// be cached.
//
// The decisive signal is that Execute succeeded: every adapter only returns
// success on a clean end of stream (EOF or [DONE]); read failures,
// timeouts and client disconnects all return errors, which travel through
// respondError and never reach the store. A missing finish reason therefore
// means "the provider closed a healthy stream", not "the stream is
// incomplete" — several OpenAI-compatible providers never send one, and
// requiring it would silently disable stream caching for them.
//
// What is still rejected: empty content, over-limit bodies, tool-call
// answers (their arguments arrive fragmented and their results live outside
// the cache key), and explicit failure finishes (error, content filter).
func streamResponseStorable(resp *providers.Response, maxBytes int) bool {
	if resp == nil {
		return false
	}
	content := resp.Content()
	if content == "" {
		return false
	}
	if maxBytes > 0 && len(content) > maxBytes {
		return false
	}
	for _, ch := range resp.Choices {
		if ch.Message != nil && len(ch.Message.ToolCalls) > 0 {
			return false
		}
	}
	switch resp.FinishReason() {
	case "", domain.FinishStop, domain.FinishLength:
		return true
	default:
		return false
	}
}

// storeCompletedStream writes a cleanly finished stream into the cache in
// the same envelope shape as a non-streaming response, so later identical
// requests hit regardless of which transport they use.
func (s *Server) storeCompletedStream(
	ctx context.Context,
	rc *domain.RequestContext,
	decision *domain.RouteDecision,
	body domain.ChatCompletionRequest,
	result *routing.ExecuteResult,
	usage domain.TokenUsage,
	cost domain.Cost,
	elapsed time.Duration,
	debug bool,
) {
	if s.cache == nil || !s.cache.Enabled() {
		return
	}
	if rc.CacheDecision == nil || !rc.CacheDecision.Cacheable {
		return
	}
	if result == nil || result.Response == nil {
		return
	}
	maxBytes := 0
	if s.config != nil {
		maxBytes = s.config.Cache.MaxResponseBytes
	}
	if !streamResponseStorable(result.Response, maxBytes) {
		return
	}
	content := result.Response.Content()
	// A requested response_format is a contract: a completion that
	// violates it is served (the stream already went out) but never
	// stored, so the violation cannot be re-served from cache.
	if rc.StructuredOutput.Mode != domain.FormatNone {
		structured := validateStructuredOutput(rc.StructuredOutput, content, false)
		if structured != nil && structured.Valid != nil && !*structured.Valid {
			return
		}
	}
	response := s.buildResponse(result.Response, rc, decision, result, usage, cost, elapsed, debug)
	if payload, merr := jsonMarshal(response); merr == nil {
		servedBy, servedModel, _, _ := servingTarget(decision, result)
		policyID, policyVersion := "", 0
		if decision != nil {
			policyID = decision.PolicyID
		}
		if rc.PolicyDecision != nil && rc.PolicyDecision.PolicyID != "" {
			policyID = rc.PolicyDecision.PolicyID
			policyVersion = rc.PolicyDecision.PolicyVersion
		}
		if servedModel == "" {
			servedModel = body.Model
		}
		s.cacheStore(ctx, rc, &body, payload, servedBy, servedModel,
			policyID, policyVersion, usage, cost.USD, result.ProviderLatencyMS)
	}
}

// streamHandler renders provider chunks as OpenAI-compatible SSE frames.
func (s *Server) streamHandler(sse *sseWriter, rc *domain.RequestContext, decision *domain.RouteDecision, debug bool) providers.StreamHandler {
	sentMeta := false

	return func(chunk providers.Chunk) error {
		// The metadata block rides on the first frame only. Repeating it on every
		// chunk would multiply the payload for no benefit, but including it once
		// means a streaming client still learns the routing that was applied.
		//
		// It carries no provider attribution on purpose. The first chunk arrives
		// while an attempt is still in progress, so the serving provider is not yet
		// known, and naming the decision's chosen target would be a guess that is
		// wrong for every failed-over stream. The terminal usage frame carries the
		// real attribution once the attempt has completed.
		var meta *domain.ResponseMetadata
		if !sentMeta {
			meta = publicMetaFrom(rc, func(m *domain.ResponseMetadata) {
				m.RoutedModel = decision.Chosen.Model
				m.RequestedModel = rc.RequestedModel
			}, debug)
			sentMeta = true
		}

		delta := chunk.Delta
		// An empty delta with a finish reason is a legitimate terminal frame, so it
		// is forwarded rather than skipped.
		frame := domain.ChatCompletionChunk{
			ID:                firstNonEmptyString(chunk.ID, rc.RequestID.String()),
			Object:            domain.ObjectChatCompletionChunk,
			Created:           chunk.Created,
			Model:             chunk.Model,
			SystemFingerprint: chunk.SystemFingerprint,
			Choices: []domain.Choice{{
				Index:        chunk.Index,
				Delta:        &delta,
				FinishReason: chunk.FinishReason,
			}},
			Synapass: meta,
		}
		if frame.Created == 0 {
			frame.Created = time.Now().Unix()
		}
		if chunk.Usage != nil {
			frame.Usage = chunk.Usage
		}
		return sse.WriteEvent(frame)
	}
}

// usageChunk builds the terminal usage frame.
//
// Because it is written after the attempt has finished, it is the one frame that
// can honestly attribute the response to a provider, so it carries the serving
// target and the fallback state.
func (s *Server) usageChunk(resp *providers.Response, rc *domain.RequestContext, decision *domain.RouteDecision, result *routing.ExecuteResult, usage domain.TokenUsage, cost domain.Cost, debug bool, completion *domain.CompletionMeta) domain.ChatCompletionChunk {
	servedBy, servedModel, fallbackUsed, attempts := servingTarget(decision, result)
	meta := publicMetaFrom(rc, func(m *domain.ResponseMetadata) {
		m.EstimatedCostUSD = cost.USD
		m.Provider = servedBy
		m.RoutedModel = servedModel
		m.FallbackUsed = fallbackUsed
		m.Attempts = attempts
		m.Completion = completion
	}, debug)
	usageCopy := usage
	return domain.ChatCompletionChunk{
		ID:         firstNonEmptyString(resp.ID, rc.RequestID.String()),
		Object:     domain.ObjectChatCompletionChunk,
		Created:    resp.Created,
		Model:      resp.Model,
		Choices:    []domain.Choice{},
		Usage:      &usageCopy,
		Synapass: meta,
	}
}

// buildResponse renders the non-streaming response body.
func (s *Server) buildResponse(
	resp *providers.Response,
	rc *domain.RequestContext,
	decision *domain.RouteDecision,
	result *routing.ExecuteResult,
	usage domain.TokenUsage,
	cost domain.Cost,
	elapsed time.Duration,
	debug bool,
) domain.ChatCompletionResponse {
	servedBy, servedModel, fallbackUsed, attempts := servingTarget(decision, result)
	meta := publicMetaFrom(rc, func(m *domain.ResponseMetadata) {
		m.Provider = servedBy
		m.RoutedModel = servedModel
		m.FallbackUsed = fallbackUsed
		m.Attempts = attempts
		m.EstimatedCostUSD = cost.USD
		m.LatencyMS = elapsed.Milliseconds()
		m.Task = string(rc.Task.Task)
		m.Shaping = rc.ShapePlan.Explain()
		if decision != nil && decision.CacheKind != "" {
			m.CacheKind = decision.CacheKind
		}
	}, debug)

	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}

	usageCopy := usage
	return domain.ChatCompletionResponse{
		ID:                firstNonEmptyString(resp.ID, rc.RequestID.String()),
		Object:            domain.ObjectChatCompletion,
		Created:           created,
		Model:             firstNonEmptyString(resp.Model, decision.Chosen.Model),
		Choices:           resp.Choices,
		Usage:             &usageCopy,
		SystemFingerprint: resp.SystemFingerprint,
		Synapass:        meta,
	}
}

// respondError writes a failure, choosing the right mechanism for whether the
// response has already been committed.
func (s *Server) respondError(
	w http.ResponseWriter,
	sse *sseWriter,
	rc *domain.RequestContext,
	err error,
	started time.Time,
	debug bool,
) {
	// A failure after the first streamed byte cannot change the status code, so it
	// is delivered as an error event inside the stream.
	if sse != nil && sse.WroteHeader() {
		normalized := domain.AsError(err)
		// A deadline that expires mid-stream is a truncation, not just a
		// failure: the client holds a partial answer. It is reported as such so
		// the metric, the log and the audit trail all say the response was cut
		// short rather than completed.
		if normalized.Code == domain.ErrCodeTimeout {
			meta := s.truncationOnStream(rc, rc.Resolution, normalized)
			s.observeTruncation(rc, rc.Resolution, meta, "timeout")
		}
		s.logger.Warn("stream failed after it started",
			"request_id", rc.RequestID.String(),
			"error", describeError(err),
		)
		if writeErr := sse.WriteStreamError(err); writeErr != nil {
			s.logger.Debug("failed to write a stream error event", "error", writeErr)
		}
		if s.metrics != nil {
			s.metrics.ObserveRequest(rc.TenantID(), "", "", string(rc.RequestType),
				domain.OutcomeError, http.StatusOK, time.Since(started).Seconds(), true)
		}
		_ = normalized

		bctx, cancel := bookkeepingContext()
		defer cancel()
		s.recordOutcome(bctx, rc, rc.Resolution, nil, domain.TokenUsage{}, domain.Cost{}, time.Since(started), http.StatusOK, err)
		return
	}

	writeError(w, err, publicMetaFrom(rc, func(m *domain.ResponseMetadata) {
		m.LatencyMS = time.Since(started).Milliseconds()
		// On a failure the provider that produced the error is the one worth naming,
		// which is not necessarily the target the router chose first.
		if normalized := domain.AsError(err); normalized != nil && normalized.Provider != "" {
			m.Provider = normalized.Provider
		}
		if rc.Resolution != nil {
			m.FallbackUsed = true
		}
	}, debug))

	bctx, cancel := bookkeepingContext()
	defer cancel()
	s.recordOutcome(bctx, rc, rc.Resolution, nil, domain.TokenUsage{}, domain.Cost{}, time.Since(started), domain.AsError(err).HTTPStatus(), err)
}

// bookkeepingContext returns a context for post-response persistence.
//
// It is deliberately detached from the request: on a client disconnect the request
// context is already cancelled, and reusing it would abort the very write that
// records the failure, which is exactly the record an operator needs most. The
// caller owns the returned cancel function.
func bookkeepingContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// recordOutcome persists the usage, log and trace for a finished request.
func (s *Server) recordOutcome(
	ctx context.Context,
	rc *domain.RequestContext,
	decision *domain.RouteDecision,
	result *routing.ExecuteResult,
	usage domain.TokenUsage,
	cost domain.Cost,
	elapsed time.Duration,
	status int,
	requestErr error,
) {
	if s.recorder == nil || rc == nil {
		return
	}

	outcome := domain.OutcomeSuccess
	errorCode := domain.ErrorCode("")
	errorMessage := ""

	if requestErr != nil {
		normalized := domain.AsError(requestErr)
		errorCode = normalized.Code
		errorMessage = normalized.Message
		switch normalized.Code {
		case domain.ErrCodeCanceled:
			outcome = domain.OutcomeCanceled
		case domain.ErrCodeInvalidRequest, domain.ErrCodeAuthentication,
			domain.ErrCodePermission, domain.ErrCodeQuotaExceeded,
			domain.ErrCodeContextLength, domain.ErrCodeNotFound,
			domain.ErrCodeContentFiltered:
			outcome = domain.OutcomeRejected
		default:
			outcome = domain.OutcomeError
		}
	} else if result != nil && result.FallbackUsed {
		outcome = domain.OutcomeFallback
	}

	// Provider identity comes from the trace, not the decision: on a fallback the
	// decision's chosen target is the primary that failed, while the trace records
	// who actually answered. Recording the wrong provider against a usage row would
	// corrupt every per-provider cost and latency report built from it.
	provider, model, fallbackUsed, attempts := servingTarget(decision, result)
	providerLatency := int64(0)
	firstToken := int64(0)
	var trace *domain.RequestTrace

	if result != nil {
		providerLatency = result.ProviderLatencyMS
		firstToken = result.FirstTokenMS
		trace = result.Trace
	}

	// Exact costing: the billed figure is recomputed here from the attempt
	// history and the resolved price sheet, replacing the routing estimate
	// for everything downstream (books, budget charge, scoring). The response
	// already sent keeps the estimate it was built with; the persisted row is
	// authoritative, which is why estimate and actual are stored side by side.
	exactCost, estimateCost, breakdown := s.buildCostBreakdown(
		ctx, rc, decision, provider, model, usage, trace, requestErr)
	cost = exactCost

	// Phase 2: feed scoring with every outcome (success and failure both inform
	// reliability). Scoring is best-effort and never blocks persistence.
	if s.scorer != nil && provider != "" {
		success := requestErr == nil
		timeout := errorCode == domain.ErrCodeTimeout
		refusal := errorCode == domain.ErrCodeContentFiltered
		s.scorer.RecordOutcome(provider, model, rc.Task.Task, success, providerLatency, cost.USD, fallbackUsed, errorCode)
		_ = timeout
		_ = refusal
	}

	out := telemetry.RequestOutcome{
		RequestID:         rc.RequestID,
		TraceID:           rc.TraceID,
		TenantID:          rc.TenantID(),
		APIKeyID:          rc.APIKeyID(),
		Provider:          provider,
		Model:             model,
		RequestedModel:    rc.RequestedModel,
		PolicyID:          policyIDOf(decision),
		RequestType:       rc.RequestType,
		Usage:             usage,
		Cost:              cost,
		EstimateCost:      estimateCost,
		PricingVersionID:  breakdown.PricingVersionID,
		PricingSource:     breakdown.PricingSource,
		Breakdown:         breakdown,
		EndpointID:        rc.EndpointID,
		LatencyMS:         elapsed.Milliseconds(),
		ProviderLatencyMS: providerLatency,
		FirstTokenMS:      firstToken,
		Attempts:          attempts,
		FallbackUsed:      fallbackUsed,
		CacheHit:          false,
		CacheLookupMS:     rc.CacheLookupMS,
		Streaming:         rc.Stream,
		Status:            status,
		Outcome:           outcome,
		ErrorCode:         errorCode,
		ErrorMessage:      errorMessage,
		ClientIP:          rc.ClientIP,
		UserAgent:         rc.UserAgent,
		EndUser:           rc.Labels["end_user"],
		Task:              rc.Task,
		Shaping:           rc.ShapePlan,
		PolicyDecision:    rc.PolicyDecision,
		Decision:          decision,
		Trace:             trace,
		Messages:          rc.Messages,
		MaxOutputTokens:   rc.MaxOutputTokens,
	}
	if rc.CacheDecision != nil && !rc.CacheDecision.Cacheable {
		out.CacheBypassReason = rc.CacheDecision.BypassReason
	}
	if decision != nil {
		out.CacheKind = decision.CacheKind
		out.ScoreNotes = decision.ScoreNotes
		out.CostBeforeUSD = decision.CostBeforeUSD
		out.CostAfterUSD = decision.CostAfterUSD
	}
	s.recorder.RecordRequest(ctx, out)

	// Spend is charged after the fact so the recorded figure reflects what was
	// actually consumed rather than what was projected.
	if s.enforcer != nil && requestErr == nil {
		if err := s.enforcer.Charge(ctx, rc, cost); err != nil {
			s.logger.Warn("failed to charge spend", "request_id", rc.RequestID.String(), "error", err)
		}
	}
}

// enforcePreflight applies rate limits and budget ceilings.
func (s *Server) enforcePreflight(ctx context.Context, rc *domain.RequestContext, pol *domain.RoutingPolicy) error {
	if s.enforcer == nil {
		return nil
	}
	// The projection is unknown before routing, so the preflight check uses the
	// policy's own ceiling as the worst case. That is conservative on purpose: a
	// tenant that cannot afford the ceiling is stopped before paying for anything.
	if err := s.enforcer.CheckPreflight(ctx, rc, pol); err != nil {
		normalized := domain.AsError(err)
		if s.metrics != nil {
			switch normalized.Code {
			case domain.ErrCodeRateLimited:
				s.metrics.ObserveRateLimited(rc.TenantID(), "gateway")
			case domain.ErrCodeQuotaExceeded:
				s.metrics.ObserveBudgetBlocked(rc.TenantID(), "policy")
			}
		}
		return err
	}
	return nil
}

// applyRoutingIntent reads the Synapass extension headers.
func applyRoutingIntent(r *http.Request, rc *domain.RequestContext) {
	if raw := r.Header.Get(headerMaxCostUSD); raw != "" {
		if value, err := strconv.ParseFloat(raw, 64); err == nil && value > 0 {
			rc.CostCeilingUSD = value
		}
	}
	if raw := r.Header.Get(headerLatencyTargetMS); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			rc.LatencyTargetMS = value
		}
	}
	if id := r.Header.Get(headerPolicyID); id != "" {
		rc.PolicyID = id
	}
}

// applyPhase2Intent reads Phase 2 extension headers: endpoint, region,
// sensitivity, batch mode and cache bypass.
func applyPhase2Intent(r *http.Request, rc *domain.RequestContext) {
	if v := r.Header.Get(headerEndpointID); v != "" {
		rc.EndpointID = v
	}
	if v := r.Header.Get(headerRegion); v != "" {
		rc.Region = v
	}
	if v := r.Header.Get(headerSensitivity); v != "" {
		for _, part := range splitComma(v) {
			if part != "" {
				rc.DataSensitivity = append(rc.DataSensitivity, part)
			}
		}
	}
	if v := r.Header.Get(headerBatch); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			rc.Batch = b
		}
	}
	if v := r.Header.Get(headerNoCache); v != "" {
		if b, err := strconv.ParseBool(v); err == nil && b {
			rc.CacheBypass = true
		}
	}
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := s[start:i]
			// trim spaces
			for len(part) > 0 && (part[0] == ' ' || part[0] == '\t') {
				part = part[1:]
			}
			for len(part) > 0 && (part[len(part)-1] == ' ' || part[len(part)-1] == '\t') {
				part = part[:len(part)-1]
			}
			out = append(out, part)
			start = i + 1
		}
	}
	return out
}

// noFallbackRequested reports whether the client opted out of failover.
func noFallbackRequested(r *http.Request) bool {
	raw := r.Header.Get(headerNoFallback)
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	return err == nil && value
}

// applyEndpointScope loads the admin-managed endpoint scope named by the
// X-Synapass-Endpoint header onto the request context.
//
// Without a header this is a no-op. With one, the scope must exist and be
// enabled: silently ignoring an unknown scope would route the request under
// different rules than the caller asked for, which is exactly the kind of
// silent misrouting that erodes trust in scopes.
func (s *Server) applyEndpointScope(ctx context.Context, rc *domain.RequestContext) error {
	if rc == nil || rc.EndpointID == "" {
		return nil
	}
	if s.repos == nil || s.repos.Endpoints == nil {
		return domain.NewError(domain.ErrCodeInternal, "endpoint scopes are unavailable")
	}
	endpoint, err := s.repos.Endpoints.GetBySlug(ctx, rc.TenantID(), rc.EndpointID)
	if err != nil {
		return err
	}
	if endpoint == nil {
		return domain.Errorf(domain.ErrCodeNotFound,
			"unknown endpoint scope %q", rc.EndpointID)
	}
	if !endpoint.Enabled {
		return domain.Errorf(domain.ErrCodePermission,
			"endpoint scope %q is disabled", rc.EndpointID)
	}
	rc.EndpointOverride = endpoint.RoutingOverride
	return nil
}

// costForUsage prices a completed request against the served target.
func costForUsage(target domain.RouteTarget, usage domain.TokenUsage) domain.Cost {
	if target.InputCostPerMillion == 0 && target.OutputCostPerMillion == 0 {
		// A local runtime with no price sheet genuinely costs nothing per token, so
		// zero is the correct answer rather than an unknown.
		return domain.Cost{}
	}
	fresh := usage.PromptTokens - min(usage.CachedPromptTokens, usage.PromptTokens)
	in := float64(fresh) / 1_000_000 * target.InputCostPerMillion
	out := float64(usage.CompletionTokens) / 1_000_000 * target.OutputCostPerMillion
	return domain.Cost{USD: in + out}
}

// handleCompletionsNotImplemented answers the legacy completions endpoint.
//
// Returning 501 with a clear message is deliberate: silently mapping it to chat
// completions would produce subtly different output for a client that expected the
// legacy format, which is worse than an explicit failure.
func (s *Server) handleCompletionsNotImplemented(w http.ResponseWriter, r *http.Request) {
	rc := requestContext(r.Context())
	writeError(w, domain.NewError(domain.ErrCodeNotImplemented,
		"the legacy /v1/completions endpoint is not supported; use /v1/chat/completions"),
		metaFromContext(rc, nil))
}

// handleEmbeddingsNotImplemented answers the embeddings endpoint.
func (s *Server) handleEmbeddingsNotImplemented(w http.ResponseWriter, r *http.Request) {
	rc := requestContext(r.Context())
	writeError(w, domain.NewError(domain.ErrCodeNotImplemented,
		"the /v1/embeddings endpoint is not enabled in this release"),
		metaFromContext(rc, nil))
}

// handleResponsesNotImplemented answers the Responses API endpoint.
//
// Like the other unimplemented OpenAI surfaces it fails explicitly: silently
// mapping it to chat completions would produce subtly different output for a
// client that expected the Responses format.
func (s *Server) handleResponsesNotImplemented(w http.ResponseWriter, r *http.Request) {
	rc := requestContext(r.Context())
	writeError(w, domain.NewError(domain.ErrCodeNotImplemented,
		"the /v1/responses endpoint is not enabled in this release; use /v1/chat/completions"),
		metaFromContext(rc, nil))
}

// firstNonEmptyString returns the first non-empty value.
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// policyIDOf returns the decision's policy id, or an empty string.
func policyIDOf(decision *domain.RouteDecision) string {
	if decision == nil {
		return ""
	}
	return decision.PolicyID
}

// errStreamNotStarted is retained so a caller can distinguish a failure before the
// stream began, which is the only case where a status code can still be sent.
var errStreamNotStarted = errors.New("stream had not started")

// runToolCompletion drives a bounded gateway-side tool run and renders the final
// answer.
//
// It reuses buildResponse so a tool-enabled response is byte-for-byte the same
// shape as a plain one; the only difference is the metadata block, which gains
// the tool run summary. Accounting happens once, at the end, over the whole
// run's usage: a run is one logical request and must not appear as N.
func (s *Server) runToolCompletion(
	ctx context.Context,
	w http.ResponseWriter,
	rc *domain.RequestContext,
	decision *domain.RouteDecision,
	toolCtx *toolContext,
	body *domain.ChatCompletionRequest,
	started time.Time,
	debug bool,
) {
	services := s.toolServices()
	runID := domain.NewID()

	executor := tools.NewExecutor(services.Invocations, toolCtx.Policy)
	completer := &toolCompleter{
		server:   s,
		rc:       rc,
		decision: decision,
		body:     body,
	}

	run, err := tools.Loop(ctx, completer, executor, services.Runs, tools.LoopOptions{
		Mode:            toolCtx.Mode,
		TenantID:        rc.TenantID(),
		RequestID:       rc.RequestID,
		Provider:        decision.Chosen.ProviderName,
		Sensitive:       toolCtx.Sensitive,
		InitialMessages: body.Messages,
		Registry:        toolCtx.Registry,
		Policy:          toolCtx.Policy,
		RunID:           runID,
	})

	if run == nil {
		s.respondError(w, nil, rc,
			domain.NewError(domain.ErrCodeInternal, "the tool run produced no result"),
			started, debug)
		return
	}

	// A run that aborted still has a final completion when the model got far
	// enough to answer; a run that never completed does not.
	if run.Final == nil || err != nil {
		if err == nil {
			err = domain.NewError(domain.ErrCodeUpstream,
				"the tool run ended without an answer")
		}
		s.respondToolError(w, rc, run, runID, toolCtx, err, started, debug)
		return
	}

	content := completionContent(run.Final)
	// A run stopped by a bound has no answer whenever the model's last move was
	// still a tool call. Emptiness is the wrong signal: models routinely attach
	// preamble text ("let me check") to a tool call, so the run looks like it
	// answered when it did not. Handing that call back would also be a dangling
	// request, because the tool result lives in this run's context rather than
	// the caller's, so the run explains itself instead.
	boundedWithoutAnswer := run.StopReason != "" && run.Final.WantsTools()
	if boundedWithoutAnswer {
		content = boundedRunNotice(run)
	}

	response, _ := run.Final.Raw.(*providers.Response)
	if response == nil {
		// The loop kept only text; render a minimal response rather than
		// losing the answer to a nil dereference.
		response = &providers.Response{Created: time.Now().Unix()}
	}

	// Structured output is checked against the requested schema. A strict schema
	// is a contract: a mismatch fails the request rather than returning content
	// the caller cannot parse.
	structuredMeta := validateStructuredOutput(toolCtx.Structured, content, false)
	if structuredMeta != nil && structuredMeta.Valid != nil && !*structuredMeta.Valid {
		s.respondToolError(w, rc, run, runID, toolCtx,
			domain.Errorf(domain.ErrCodeInvalidRequest,
				"the response did not match the requested JSON schema: %s",
				structuredMeta.Error), started, debug)
		return
	}

	usage := completer.usage
	if usage.TotalTokens == 0 {
		usage = domain.TokenUsage{PromptTokens: rc.PromptTokens, Estimated: true}.Normalize()
	}
	cost := completer.cost
	elapsed := time.Since(started)

	servedBy, servedModel, fallbackUsed, attempts := toolServingTarget(decision)
	meta := publicMetaFrom(rc, func(m *domain.ResponseMetadata) {
		m.Provider = servedBy
		m.RoutedModel = servedModel
		m.FallbackUsed = fallbackUsed
		m.Attempts = attempts
		m.EstimatedCostUSD = cost.USD
		m.LatencyMS = elapsed.Milliseconds()
		m.Task = string(rc.Task.Task)
		m.ToolRun = toolRunMetadata(run, runID, toolCtx.Mode)
		m.ToolRun.Structured = structuredMeta
	}, debug)

	created := response.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	usageCopy := usage
	writeJSON(w, http.StatusOK, domain.ChatCompletionResponse{
		ID:         firstNonEmptyString(response.ID, rc.RequestID.String()),
		Object:     domain.ObjectChatCompletion,
		Created:    created,
		Model:      firstNonEmptyString(response.Model, decision.Chosen.Model),
		Choices:    []domain.Choice{toolFinalChoiceBounded(response, content, boundedWithoutAnswer)},
		Usage:      &usageCopy,
		Synapass: meta,
	})

	s.recordOutcome(ctx, rc, decision, nil, usage, cost, elapsed, http.StatusOK, nil)
	s.auditToolRun(rc, run, runID, toolCtx.Mode, true, "")
}

// boundedRunNotice explains a run that a policy bound stopped before the model
// produced an answer.
//
// It names what was executed, because that work really happened and the caller
// has no other way to learn it: the tool results were injected into the
// gateway's run rather than returned to the client.
func boundedRunNotice(run *tools.LoopResult) string {
	executed := make([]string, 0, len(run.Invocations))
	for _, invocation := range run.Invocations {
		if invocation.Status == domain.InvocationExecuted {
			executed = append(executed, invocation.ToolName)
		}
	}

	var b strings.Builder
	b.WriteString("The tool run stopped before the model produced an answer: ")
	b.WriteString(run.StopReason)
	b.WriteString(".")
	switch {
	case len(executed) == 0:
		b.WriteString(" No tool was executed.")
	case len(executed) == 1:
		b.WriteString(" Executed " + executed[0] + ".")
	default:
		b.WriteString(fmt.Sprintf(" Executed %d tools: %s.",
			len(executed), strings.Join(executed, ", ")))
	}
	b.WriteString(" Their results were applied inside the gateway, so the run cannot be resumed by this client; raise the limit in the tool policy, or set tool_execution.mode to manual to run tools yourself.")
	return b.String()
}

// toolFinalChoice renders the run's final answer as a normal completion choice.
func toolFinalChoice(response *providers.Response, content string) domain.Choice {
	finish := domain.FinishStop
	if len(response.Choices) > 0 && response.Choices[0].FinishReason != nil {
		finish = *response.Choices[0].FinishReason
	}
	return domain.Choice{
		Index:        0,
		Message:      &domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(content)},
		FinishReason: &finish,
	}
}

// toolFinalChoiceBounded is toolFinalChoice for a run the gateway finished
// itself. The answer is already resolved, so it ends with "stop" rather than
// leaving a tool_calls finish reason attached to text that needs no tool.
func toolFinalChoiceBounded(response *providers.Response, content string, bounded bool) domain.Choice {
	choice := toolFinalChoice(response, content)
	if bounded {
		finish := domain.FinishStop
		choice.FinishReason = &finish
	}
	return choice
}

// completionContent extracts the final answer text from a completion.
func completionContent(completion *tools.Completion) string {
	if completion == nil {
		return ""
	}
	return completion.Content
}

// toolServingTarget reports who served the run. A multi-step run reuses the
// request's decision, so the chosen target is the honest answer.
func toolServingTarget(decision *domain.RouteDecision) (provider, model string, fallbackUsed bool, attempts int) {
	if decision == nil {
		return "", "", false, 0
	}
	return decision.Chosen.ProviderName, decision.Chosen.Model, false, 1
}

// respondToolError records a failed run before rendering the error, so a run
// that stopped early still leaves a trace.
func (s *Server) respondToolError(
	w http.ResponseWriter,
	rc *domain.RequestContext,
	run *tools.LoopResult,
	runID string,
	toolCtx *toolContext,
	err error,
	started time.Time,
	debug bool,
) {
	meta := publicMetaFrom(rc, func(m *domain.ResponseMetadata) {
		m.LatencyMS = time.Since(started).Milliseconds()
		m.ToolRun = toolRunMetadata(run, runID, toolCtx.Mode)
	}, debug)
	writeError(w, err, meta)

	ctx, cancel := bookkeepingContext()
	defer cancel()
	s.recordOutcome(ctx, rc, rc.Resolution, nil,
		domain.TokenUsage{PromptTokens: rc.PromptTokens, Estimated: true}.Normalize(),
		domain.Cost{}, time.Since(started), domain.AsError(err).HTTPStatus(), err)
	s.auditToolRun(rc, run, runID, toolCtx.Mode, false, err.Error())
}

// auditToolRun writes the audit event for a bounded run, and fans the same
// outcome out to metrics and NATS.
//
// Runs are audited rather than only recorded: "the gateway executed tools on
// this request" is exactly the kind of fact an operator has to be able to prove
// after the fact. Metrics and the event carry the same summary the audit row
// does, so the dashboard, the alerts and the log cannot disagree.
func (s *Server) auditToolRun(
	rc *domain.RequestContext,
	run *tools.LoopResult,
	runID string,
	mode domain.ToolPolicyMode,
	success bool,
	failure string,
) {
	if run == nil {
		return
	}
	// A run whose trace failed to persist must say so in the log, or an
	// operator investigating a missing run has nothing to go on.
	if run.PersistError != nil {
		s.logger.Warn("failed to persist the tool run trace",
			"run_id", runID,
			"error", run.PersistError.Error(),
		)
	}
	if s.metrics != nil {
		tenant := ""
		if rc != nil {
			tenant = rc.TenantID()
		}
		s.metrics.ObserveToolRun(tenant, string(mode), run.Status,
			float64(run.LatencyMS)/1000, run.Invocations, run.PersistError)
	}
	if s.nats != nil {
		event := &domain.ToolRunEvent{
			RunID:     runID,
			Success:   success,
			Run:       *toolRunMetadata(run, runID, mode),
			Provider:  run.Provider,
			Model:     run.Model,
			Persisted: run.PersistError == nil,
		}
		if rc != nil {
			event.RequestID = rc.RequestID.String()
			event.TenantID = rc.TenantID()
		}
		s.nats.PublishToolRunCompleted(event)
	}
	summary := map[string]any{
		"run_id":     runID,
		"steps":      run.Steps,
		"tool_calls": run.ToolCalls,
		"status":     string(run.Status),
	}
	if run.StopReason != "" {
		summary["stop_reason"] = run.StopReason
	}
	if failure != "" {
		summary["error"] = failure
	}
	names := make([]string, 0, len(run.Invocations))
	executed := 0
	for _, invocation := range run.Invocations {
		names = append(names, invocation.ToolName)
		if invocation.Status == domain.InvocationExecuted {
			executed++
		}
	}
	if len(names) > 0 {
		summary["tools"] = names
	}
	summary["executed"] = executed

	if rc == nil {
		return
	}
	ctx, cancel := bookkeepingContext()
	defer cancel()
	action := domain.AuditExecute
	if !success {
		action = domain.AuditDeny
	}
	s.audit(ctx, rc, action, domain.ResourceAgentRun, runID, nil, summary)
}
