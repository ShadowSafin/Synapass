package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/shadowsafin/synapass/internal/domain"
)

// KeyInput is everything that distinguishes cacheable requests.
//
// Two requests share an exact entry only when every field that can change
// the answer matches: tenant, key, model, full message content (including
// system/developer turns), tool definitions, generation settings, response
// contract, routing policy and sensitivity. Anything less is how one tenant
// serves another tenant's answer.
type KeyInput struct {
	TenantID    string
	APIKeyID    string
	Model       string
	Provider    string
	Messages    []domain.ChatMessage
	Tools       []domain.Tool
	ToolChoice  string
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	TopK        *int
	MinP        *float64
	Repetition  *float64
	Presence    *float64
	Frequency   *float64
	Seed        *int
	N           int
	FormatType  string
	FormatSchema string
	Reasoning   string
	Stop        string
	PolicyID    string
	PolicyVersion int
	EndpointID  string
	Sensitivity string
	User        string
}

// PromptHash is the stable hash of the normalized conversation.
func (in KeyInput) PromptHash() string {
	h := sha256.New()
	for _, m := range in.Messages {
		h.Write([]byte(string(m.Role.Normalize())))
		h.Write([]byte{0})
		h.Write([]byte(normalize(m.Text())))
		h.Write([]byte{0})
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				h.Write([]byte(tc.Function.Name))
				h.Write([]byte{0})
				// Arguments are canonicalized so equivalent JSON with
				// different field order or whitespace shares a key.
				h.Write([]byte(canonicalJSONString(tc.Function.Arguments)))
				h.Write([]byte{0})
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ToolsHash covers full tool definitions, not just names: a changed schema
// must not reuse an answer produced under the old one.
func (in KeyInput) ToolsHash() string {
	if len(in.Tools) == 0 && in.ToolChoice == "" {
		return ""
	}
	names := make([]string, 0, len(in.Tools))
	byName := map[string]domain.Tool{}
	for _, t := range in.Tools {
		names = append(names, t.Function.Name)
		byName[t.Function.Name] = t
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		t := byName[n]
		h.Write([]byte(t.Type))
		h.Write([]byte{0})
		h.Write([]byte(t.Function.Name))
		h.Write([]byte{0})
		h.Write([]byte(t.Function.Description))
		h.Write([]byte{0})
		// Parameters are canonicalized: the same schema with reordered
		// keys or different whitespace must map to the same entry, while
		// a real schema change must still isolate.
		h.Write([]byte(canonicalJSONBytes(t.Function.Parameters)))
		h.Write([]byte{0})
		if t.Function.Strict != nil && *t.Function.Strict {
			h.Write([]byte("strict=1"))
			h.Write([]byte{0})
		}
	}
	h.Write([]byte("choice=" + canonicalJSONString(in.ToolChoice)))
	return hex.EncodeToString(h.Sum(nil))
}

// SettingsHash covers generation settings that change the distribution.
func (in KeyInput) SettingsHash() string {
	var b strings.Builder
	if in.MaxTokens > 0 {
		b.WriteString("max=" + itoa(in.MaxTokens) + ";")
	}
	if in.Temperature != nil {
		b.WriteString("temp=" + floatStr(*in.Temperature) + ";")
	}
	if in.TopP != nil {
		b.WriteString("topp=" + floatStr(*in.TopP) + ";")
	}
	if in.TopK != nil {
		b.WriteString("topk=" + itoa(*in.TopK) + ";")
	}
	if in.MinP != nil {
		b.WriteString("minp=" + floatStr(*in.MinP) + ";")
	}
	if in.Repetition != nil {
		b.WriteString("rep=" + floatStr(*in.Repetition) + ";")
	}
	if in.Presence != nil {
		b.WriteString("pres=" + floatStr(*in.Presence) + ";")
	}
	if in.Frequency != nil {
		b.WriteString("freq=" + floatStr(*in.Frequency) + ";")
	}
	if in.Seed != nil {
		b.WriteString("seed=" + itoa(*in.Seed) + ";")
	}
	if in.N > 0 {
		b.WriteString("n=" + itoa(in.N) + ";")
	}
	if in.FormatType != "" {
		b.WriteString("fmt=" + in.FormatType + ";")
	}
	if in.FormatSchema != "" {
		sum := sha256.Sum256(canonicalJSONBytes(json.RawMessage(in.FormatSchema)))
		b.WriteString("schema=" + hex.EncodeToString(sum[:]) + ";")
	}
	if in.Reasoning != "" {
		b.WriteString("reason=" + in.Reasoning + ";")
	}
	if in.Stop != "" {
		sum := sha256.Sum256(canonicalJSONBytes(json.RawMessage(in.Stop)))
		b.WriteString("stop=" + hex.EncodeToString(sum[:]) + ";")
	}
	return b.String()
}

// ExactKey hashes the full normalized request. The tenant and key scope the
// namespace; the hash covers content, tools, settings, contract and policy.
func ExactKey(in KeyInput) string {
	h := sha256.New()
	h.Write([]byte("tenant=" + in.TenantID))
	h.Write([]byte{0})
	h.Write([]byte("key=" + in.APIKeyID))
	h.Write([]byte{0})
	h.Write([]byte("model=" + strings.ToLower(strings.TrimSpace(in.Model))))
	h.Write([]byte{0})
	if in.Provider != "" {
		h.Write([]byte("provider=" + in.Provider))
		h.Write([]byte{0})
	}
	h.Write([]byte("prompt=" + in.PromptHash()))
	h.Write([]byte{0})
	if th := in.ToolsHash(); th != "" {
		h.Write([]byte("tools=" + th))
		h.Write([]byte{0})
	}
	if sh := in.SettingsHash(); sh != "" {
		h.Write([]byte("settings=" + sh))
		h.Write([]byte{0})
	}
	if in.PolicyID != "" {
		h.Write([]byte("policy=" + in.PolicyID))
		h.Write([]byte{0})
		if in.PolicyVersion > 0 {
			h.Write([]byte("policyv=" + itoa(in.PolicyVersion)))
			h.Write([]byte{0})
		}
	}
	if in.EndpointID != "" {
		h.Write([]byte("endpoint=" + in.EndpointID))
		h.Write([]byte{0})
	}
	if in.Sensitivity != "" {
		h.Write([]byte("sens=" + in.Sensitivity))
		h.Write([]byte{0})
	}
	if in.User != "" {
		h.Write([]byte("user=" + in.User))
		h.Write([]byte{0})
	}
	return "exact:" + hex.EncodeToString(h.Sum(nil))
}

// PrefixKey hashes the stable system/developer prefix plus the tenant/model
// scope. It is used for conversation-aware reuse accounting and for short
// prompts only — never as a full-response key for divergent suffixes.
func PrefixKey(in KeyInput, length int) string {
	if length <= 0 {
		length = 256
	}
	var b strings.Builder
	for _, m := range in.Messages {
		if m.Role != domain.RoleSystem && m.Role != domain.RoleDeveloper {
			continue
		}
		b.WriteString(normalize(m.Text()))
		b.WriteByte('\n')
		if b.Len() >= length {
			break
		}
	}
	// Fall back to the leading normalized conversation when no system prefix
	// exists, so prefix accounting still works for template-heavy traffic.
	if b.Len() == 0 {
		for _, m := range in.Messages {
			b.WriteString(normalize(m.Text()))
			b.WriteByte('\n')
			if b.Len() >= length {
				break
			}
		}
	}
	s := b.String()
	if len(s) > length {
		s = s[:length]
	}
	// The model is lower-cased and trimmed to match ExactKey: "GPT-4o"
	// and "gpt-4o" are the same model and must share a prefix entry.
	h := sha256.Sum256([]byte(in.TenantID + "\x00" + in.APIKeyID + "\x00" + strings.ToLower(strings.TrimSpace(in.Model)) + "\x00" + s))
	return "prefix:" + hex.EncodeToString(h[:])
}

// SystemPrefixHash isolates the stable instruction prefix for
// conversation-aware caching: long chats sharing one system prompt share
// preparation work without sharing answers.
func SystemPrefixHash(in KeyInput) string {
	var b strings.Builder
	for _, m := range in.Messages {
		if m.Role != domain.RoleSystem && m.Role != domain.RoleDeveloper {
			continue
		}
		b.WriteString(normalize(m.Text()))
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// NormalizedLength returns the normalized full-prompt length. Prefix-tier
// response reuse is only allowed when this fits inside PrefixLength, which
// is what makes the tier safe: a shared 256-char head with a different tail
// is never served as a hit.
func (in KeyInput) NormalizedLength() int {
	n := 0
	for _, m := range in.Messages {
		n += len(normalize(m.Text())) + 1
	}
	return n
}

// PromptPreview returns a short human preview for the dashboard top-prompts
// view. It carries no secrets beyond what the operator already stores.
func (in KeyInput) PromptPreview(max int) string {
	if max <= 0 {
		max = 120
	}
	var b strings.Builder
	for _, m := range in.Messages {
		if m.Role != domain.RoleUser {
			continue
		}
		if t := strings.TrimSpace(m.Text()); t != "" {
			if b.Len() > 0 {
				b.WriteString(" / ")
			}
			b.WriteString(strings.Join(strings.Fields(t), " "))
		}
	}
	s := b.String()
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// KeyInputFromRequest builds the full key input from a chat request and its
// routing context. Provider is left empty pre-route; the hit validator
// checks serving metadata after the route resolves.
func KeyInputFromRequest(tenantID, apiKeyID, model string, req *domain.ChatCompletionRequest, policyID string, policyVersion int, endpointID, sensitivity, user string) KeyInput {
	in := KeyInput{TenantID: tenantID, APIKeyID: apiKeyID, Model: model}
	if req == nil {
		return in
	}
	in.Messages = req.Messages
	in.Tools = req.Tools
	if len(req.ToolChoice) > 0 {
		in.ToolChoice = string(req.ToolChoice)
	}
	in.MaxTokens = req.RequestedMaxTokens()
	in.Temperature = req.Temperature
	in.TopP = req.TopP
	in.TopK = req.TopK
	in.MinP = req.MinP
	in.Repetition = req.RepetitionPenalty
	in.Presence = req.PresencePenalty
	in.Frequency = req.FrequencyPenalty
	in.Seed = req.Seed
	if req.N != nil {
		in.N = *req.N
	}
	if req.ResponseFormat != nil {
		in.FormatType = req.ResponseFormat.Type
		if len(req.ResponseFormat.JSONSchema) > 0 {
			in.FormatSchema = string(req.ResponseFormat.JSONSchema)
		}
	}
	in.Reasoning = req.ReasoningEffort
	if len(req.Stop) > 0 {
		in.Stop = string(req.Stop)
	}
	in.PolicyID = policyID
	in.PolicyVersion = policyVersion
	in.EndpointID = endpointID
	in.Sensitivity = sensitivity
	in.User = user
	return in
}

// formatSchemaHash extracts a stable schema hash for keying without storing
// the schema itself.
func formatSchemaHash(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(canonicalJSONBytes(raw))
	return hex.EncodeToString(sum[:])
}

// canonicalJSONBytes re-encodes JSON with sorted keys and no insignificant
// whitespace, so logically identical payloads (tool arguments, schemas,
// stop sequences) share a cache key regardless of field order or
// formatting. Non-JSON input is returned trimmed as-is; it still hashes
// deterministically, it just does not gain order-insensitivity.
func canonicalJSONBytes(raw json.RawMessage) []byte {
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return []byte(t)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return []byte(t)
	}
	return out
}

// canonicalJSONString canonicalizes a JSON string payload for keying.
func canonicalJSONString(s string) string {
	if s == "" {
		return ""
	}
	return string(canonicalJSONBytes(json.RawMessage(s)))
}
