package admin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

// capabilityDoer is a minimal HTTPDoer so this test can drive the real adapter.
type capabilityDoer struct {
	paths []string
}

func (d *capabilityDoer) Do(req *http.Request) (*http.Response, error) {
	d.paths = append(d.paths, req.URL.Path)
	body := `{"object":"list","data":[{"id":"agnes-3-flash"}]}`
	switch req.URL.Path {
	case "/model/info":
		// The LiteLM shape, served at the root as LiteLM does.
		body = `{"data":[{"model_name":"agnes-3-flash","model_info":{` +
			`"supports_function_calling":true,"supports_parallel_function_calling":true,` +
			`"supports_streaming":true,"supports_vision":true,` +
			`"supports_response_schema":true}}]}`
	case "/v1/models":
		// as above: ids only
	default:
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader(`{"error":"not found"}`)),
			Header:     http.Header{},
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// The whole chain, for real: the actual OpenAI adapter, the actual catalogue
// fetch, the actual sync. Nothing stubbed above the HTTP boundary.
//
// This is the regression test for the reported failure — a provider that never
// declared capabilities, whose models therefore inherited a default that had no
// tools, so every tool request was filtered out before it left the gateway.
func TestSyncModelsLearnsCapabilitiesFromRealAdapter(t *testing.T) {
	doer := &capabilityDoer{}
	adapter, err := providers.NewAdapter(domain.Provider{
		ID:      "p1",
		Name:    "bynara",
		Kind:    domain.ProviderOpenAICompatible,
		BaseURL: "https://router.example/v1",
		Status:  domain.StatusActive,
	}, providers.Options{

		Client:           doer,
		MaxResponseBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("construct adapter: %v", err)
	}

	store := &memModelStore{}
	res, err := SyncModels(context.Background(), adapter, store, SyncOptions{
		ProviderID: "p1", Environment: domain.EnvTest,
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	if res.CapabilitySource != CapabilitySourceProvider {
		t.Fatalf("capability_source = %q, want %q (requests: %v)",
			res.CapabilitySource, CapabilitySourceProvider, doer.paths)
	}
	if len(res.Created) != 1 || res.Created[0] != "agnes-3-flash" {
		t.Fatalf("created = %v", res.Created)
	}
	if res.CapabilitiesFilled != 1 {
		t.Fatalf("capabilities_filled = %d, want 1", res.CapabilitiesFilled)
	}

	row := store.rows[len(store.rows)-1]
	caps := row.CapabilitySet()
	for _, want := range []domain.Capability{domain.CapTools, domain.CapParallelTool, domain.CapVision, domain.CapJSONSchema} {
		if !caps.Contains(want) {
			t.Errorf("model is missing %q; got %v", want, row.Capabilities)
		}
	}
	if row.Metadata[CapabilitiesSourceKey] != CapabilitiesSourceProvider {
		t.Errorf("capabilities_source metadata = %q", row.Metadata[CapabilitiesSourceKey])
	}
}

// The root spelling must be reached, because that is where LiteLM serves it and
// the configured base URL ends in /v1.
func TestSyncModelsReachesRootModelInfo(t *testing.T) {
	doer := &capabilityDoer{}
	adapter, err := providers.NewAdapter(domain.Provider{
		ID: "p1", Name: "r", Kind: domain.ProviderOpenAICompatible,
		BaseURL: "https://router.example/v1", Status: domain.StatusActive,
	}, providers.Options{Client: doer, MaxResponseBytes: 1 << 20})
	if err != nil {
		t.Fatalf("construct adapter: %v", err)
	}
	if _, err := SyncModels(context.Background(), adapter, &memModelStore{}, SyncOptions{ProviderID: "p1"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	saw := false
	for _, p := range doer.paths {
		if p == "/model/info" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("root /model/info was never attempted; requests: %v", doer.paths)
	}
}
