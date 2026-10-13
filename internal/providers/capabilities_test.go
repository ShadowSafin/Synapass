package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

func TestParseModelInfoLayouts(t *testing.T) {
	yes, no := true, false
	_ = no

	cases := []struct {
		name  string
		body  string
		want  []string
		tools bool
	}{
		{
			name:  "data array with model_info",
			body:  `{"data":[{"model_name":"m1","model_info":{"supports_function_calling":true,"supports_streaming":true}}]}`,
			want:  []string{"m1"},
			tools: true,
		},
		{
			name:  "data.model_list",
			body:  `{"data":{"model_list":[{"model_name":"m2","model_info":{"supports_tool_calling":true}}]}}`,
			want:  []string{"m2"},
			tools: true,
		},
		{
			name:  "bare array",
			body:  `[{"model_name":"m3","model_info":{"supports_function_calling":true}}]`,
			want:  []string{"m3"},
			tools: true,
		},
		{
			name:  "model_name absent falls back to litellm_params",
			body:  `{"data":[{"litellm_params":{"model":"m4"},"model_info":{"supports_function_calling":true}}]}`,
			want:  []string{"m4"},
			tools: true,
		},
		{
			// The whole point: silence must not become a negative claim.
			name:  "silent model_info yields no capabilities",
			body:  `{"data":[{"model_name":"m5","model_info":{}}]}`,
			want:  []string{"m5"},
			tools: false,
		},
		{
			name:  "explicit false is also not recorded",
			body:  `{"data":[{"model_name":"m6","model_info":{"supports_function_calling":false}}]}`,
			want:  []string{"m6"},
			tools: false,
		},
		{
			name:  "ollama-style literal capability list",
			body:  `[{"name":"m7","capabilities":["tools","vision"]}]`,
			want:  []string{"m7"},
			tools: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseModelInfo([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseModelInfo: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("models = %+v, want %v", got, tc.want)
			}
			for i, name := range tc.want {
				if got[i].Name != name {
					t.Errorf("model[%d] = %q, want %q", i, got[i].Name, name)
				}
			}
			hasTools := false
			for _, c := range got[0].Capabilities {
				if c == domain.CapTools {
					hasTools = true
				}
			}
			if hasTools != tc.tools {
				t.Errorf("tools present = %v, want %v (caps %v)", hasTools, tc.tools, got[0].Capabilities)
			}
		})
	}
	_ = yes
}

func TestParseModelInfoRejectsUnknownShapes(t *testing.T) {
	for _, body := range []string{`{"object":"list","data":[{"id":"x"}]}`, `not json`, `{}`} {
		if _, err := parseModelInfo([]byte(body)); err == nil {
			t.Errorf("body %q: expected errModelInfoUnsupported, got nil", body)
		}
	}
}

// capabilityListerStub stands in for an adapter that publishes /model/info.
type capabilityListerStub struct {
	*openAIAdapter
	models []RemoteModel
}

func (s capabilityListerStub) ListModelsWithCapabilities(context.Context) ([]RemoteModel, error) {
	return s.models, nil
}

func TestOpenAIAdapterListModelsWithCapabilitiesUsesModelInfo(t *testing.T) {
	// The base URL ends in /v1, which is the case that matters: LiteLM serves
	// /model/info at the root, so the base-relative path must be tried too.
	transport := newStubTransport(func(r *http.Request) *http.Response {
		// The relative attempt resolves under /v1; the root attempt does not.
		if r.URL.Path == "/model/info" || r.URL.Path == "/v1/model/info" {
			return jsonBody(`{"data":[{"model_name":"x","model_info":{"supports_function_calling":true}}]}`)
		}
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader(`{"error":"not found"}`)),
			Header:     http.Header{},
		}
	})
	adapter := &openAIAdapter{baseAdapter: &baseAdapter{
		provider: domain.Provider{Name: "r", Kind: domain.ProviderOpenAICompatible},
		client:   transport,
		baseURL:  "https://router.example/v1",
	}}

	models, err := adapter.ListModelsWithCapabilities(context.Background())
	if err != nil {
		t.Fatalf("ListModelsWithCapabilities: %v", err)
	}
	if len(transport.requests) == 0 {
		t.Fatal("no request was made")
	}
	for i, r := range transport.requests {
		t.Logf("attempt %d: %s", i+1, r.URL.Path)
	}
	if len(models) != 1 || len(models[0].Capabilities) != 1 || models[0].Capabilities[0] != domain.CapTools {
		t.Fatalf("unexpected models: %+v", models)
	}
}

// The root path must be attempted after the version-suffixed one fails, since
// that is the spelling LiteLM actually serves.
func TestModelInfoPathsTriesBaseThenRoot(t *testing.T) {
	got := modelInfoPaths("https://router.example/v1")
	if len(got) != 2 || got[0] != "/model/info" || got[1] != "https://router.example/model/info" {
		t.Fatalf("paths = %v", got)
	}
	// A base with no version suffix needs only the relative path.
	if one := modelInfoPaths("https://api.example.com"); len(one) != 1 {
		t.Fatalf("paths = %v, want a single attempt", one)
	}
}

// A provider with no such endpoint must not look like a failure: discovery
// falls back silently, so this has to come back as the sentinel.
func TestOpenAIAdapterListModelsWithCapabilitiesAbsentIsNotFatal(t *testing.T) {
	adapter := &openAIAdapter{baseAdapter: &baseAdapter{
		provider: domain.Provider{Name: "r", Kind: domain.ProviderOpenAICompatible},
		client: newStubTransport(func(*http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader(`{"error":"not found"}`)),
				Header:     http.Header{},
			}
		}),
	}}

	if _, err := adapter.ListModelsWithCapabilities(context.Background()); err == nil {
		t.Fatal("expected the unsupported sentinel, got nil")
	}
}

// The id listing must keep working exactly as before; the rich path is additive.
func TestOpenAIAdapterListModelsUnchanged(t *testing.T) {
	adapter := &openAIAdapter{baseAdapter: &baseAdapter{
		provider: domain.Provider{Name: "r", Kind: domain.ProviderOpenAI},
		client: newStubTransport(func(*http.Request) *http.Response {
			return jsonBody(`{"data":[{"id":"a"},{"id":"b"}]}`)
		}),
	}}

	names, err := adapter.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names = %v", names)
	}
}

// Compile-time proof the adapter satisfies the optional interface.
var _ ModelCapabilityLister = (*openAIAdapter)(nil)

func jsonBody(s string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(s)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}
