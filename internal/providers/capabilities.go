package providers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/shadowsafin/synapass/internal/domain"
)

// errModelInfoUnsupported means the provider does not publish capability
// metadata. It is an ordinary outcome, not a failure: most OpenAI-compatible
// servers expose only /v1/models, and discovery carries on without it.
var errModelInfoUnsupported = errors.New("provider publishes no capability metadata")

// RemoteModel is one entry from a provider's remote catalogue.
type RemoteModel struct {
	// Name is the identifier the provider expects on the wire.
	Name string
	// Capabilities is what the provider itself claims this model can do. Empty
	// means the catalogue said nothing, which is the common case and is not an
	// error: it just leaves the kind default in charge.
	Capabilities []domain.Capability
}

// ModelCapabilityLister is an optional interface for adapters whose remote
// catalogue carries per-model capability metadata.
//
// It exists because the OpenAI /v1/models response carries only an id, so
// discovery cannot learn anything about a model from it. Routers built on
// LiteLM and its many derivatives publish a richer, free, zero-token view at
// /model/info; reading it when it exists is strictly better than guessing.
//
// Adapters that do not implement this fall back to the bare id listing, which
// stays the supported path. Nothing here is required for discovery to work.
type ModelCapabilityLister interface {
	ListModelsWithCapabilities(ctx context.Context) ([]RemoteModel, error)
}

// capabilityClaims is the shape a router uses to describe a model. Every field
// is optional: a provider that reports only some of them is still useful, and a
// missing field is left out rather than guessed.
type capabilityClaims struct {
	SupportsFunctionCalling *bool `json:"supports_function_calling"`
	SupportsToolCalling     *bool `json:"supports_tool_calling"`
	SupportsParallelCalls   *bool `json:"supports_parallel_function_calling"`
	SupportsStreaming       *bool `json:"supports_streaming"`
	SupportsVision          *bool `json:"supports_vision"`
	SupportsResponseSchema  *bool `json:"supports_response_schema"`
	SupportsJSONSchema      *bool `json:"supports_json_schema"`
	SupportsJSONObject      *bool `json:"supports_json_object"`
}

// modelInfoEntry is one row of a /model/info response. LiteLM and its
// derivatives disagree about whether the key is model_name or nested under
// litellm_params, so both are read.
type modelInfoEntry struct {
	ModelName    string           `json:"model_name"`
	ModelInfo    capabilityClaims `json:"model_info"`
	LiteLLMParam struct {
		Model string `json:"model"`
	} `json:"litellm_params"`
	// Name is Ollama's key for the same thing.
	Name string `json:"name"`
	// Capabilities is Ollama's literal capability list rather than booleans.
	Capabilities []domain.Capability `json:"capabilities"`
}

// modelInfoResponse covers the layouts seen in the wild: a bare array, a
// `data` array, or a `data` object holding `model_list`.
type modelInfoResponse struct {
	Data json.RawMessage `json:"data"`
}

// claimsFrom converts a provider's claims into capabilities.
//
// Only affirmative claims become capabilities. An absent or false field is
// omitted rather than recorded as a negative, because an absent field means the
// catalogue is silent, not that the model is incapable — and silently recording
// "no tools" from a missing field is exactly the bug this work exists to fix.
func claimsFrom(c capabilityClaims) []domain.Capability {
	var out []domain.Capability
	add := func(yes *bool, cap domain.Capability) {
		if yes != nil && *yes {
			out = append(out, cap)
		}
	}
	add(c.SupportsFunctionCalling, domain.CapTools)
	add(c.SupportsToolCalling, domain.CapTools)
	add(c.SupportsParallelCalls, domain.CapParallelTool)
	add(c.SupportsStreaming, domain.CapStreaming)
	add(c.SupportsVision, domain.CapVision)
	add(c.SupportsResponseSchema, domain.CapJSONSchema)
	add(c.SupportsJSONSchema, domain.CapJSONSchema)
	add(c.SupportsJSONObject, domain.CapJSONMode)
	return out
}

// name returns the identifier to route on. LiteLM and its derivatives disagree
// about which key holds it, and Ollama uses a third, so all three are read.
func (e modelInfoEntry) name() string {
	switch {
	case e.ModelName != "":
		return e.ModelName
	case e.LiteLLMParam.Model != "":
		return e.LiteLLMParam.Model
	default:
		return e.Name
	}
}

// parseModelInfo decodes whichever of the known /model/info layouts it is given.
//
// A body that parses but yields no usable entries is reported as unsupported
// rather than as an empty success: that is what a plain /v1/models payload
// looks like if a provider ever serves it at this path, and the caller needs to
// fall back rather than conclude the provider has no models.
func parseModelInfo(body []byte) ([]RemoteModel, error) {
	// Shape 1: a bare array of entries.
	var bare []modelInfoEntry
	if err := json.Unmarshal(body, &bare); err == nil {
		return usable(bare)
	}

	var wrapper modelInfoResponse
	if err := json.Unmarshal(body, &wrapper); err != nil || len(wrapper.Data) == 0 {
		return nil, errModelInfoUnsupported
	}

	// Shape 2: {"data": [...]}
	var arr []modelInfoEntry
	if err := json.Unmarshal(wrapper.Data, &arr); err == nil {
		return usable(arr)
	}

	// Shape 3: {"data": {"model_list": [...]}}
	var obj struct {
		ModelList []modelInfoEntry `json:"model_list"`
	}
	if err := json.Unmarshal(wrapper.Data, &obj); err != nil {
		return nil, errModelInfoUnsupported
	}
	return usable(obj.ModelList)
}

func usable(entries []modelInfoEntry) ([]RemoteModel, error) {
	out := entriesToRemote(entries)
	if len(out) == 0 {
		return nil, errModelInfoUnsupported
	}
	return out, nil
}

func entriesToRemote(entries []modelInfoEntry) []RemoteModel {
	out := make([]RemoteModel, 0, len(entries))
	for _, e := range entries {
		name := e.name()
		if strings.TrimSpace(name) == "" {
			continue
		}
		caps := claimsFrom(e.ModelInfo)
		if len(caps) == 0 && len(e.Capabilities) > 0 {
			// Ollama-style: the provider lists capabilities directly.
			caps = append(caps, e.Capabilities...)
		}
		out = append(out, RemoteModel{Name: name, Capabilities: caps})
	}
	return out
}
