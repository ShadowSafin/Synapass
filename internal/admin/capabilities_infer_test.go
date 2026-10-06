package admin

import (
	"context"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

// Known families resolve without a single upstream call.
func TestInferCapabilitiesKnownFamilies(t *testing.T) {
	cases := []struct {
		name string
		want []domain.Capability
	}{
		{"claude-fable-5-1", []domain.Capability{domain.CapChat, domain.CapTools, domain.CapVision, domain.CapStreaming}},
		{"MiniMax-M3", []domain.Capability{domain.CapChat, domain.CapTools, domain.CapStreaming}},
		{"gpt-5.6-sol", []domain.Capability{domain.CapChat, domain.CapTools, domain.CapVision, domain.CapJSONSchema}},
		{"gemini-3.5-flash", []domain.Capability{domain.CapChat, domain.CapTools, domain.CapVision}},
		{"deepseek-v4-pro", []domain.Capability{domain.CapChat, domain.CapTools, domain.CapReasoning}},
		{"grok-4.20", []domain.Capability{domain.CapChat, domain.CapTools, domain.CapVision}},
		{"kimi-k3", []domain.Capability{domain.CapChat, domain.CapTools, domain.CapVision}},
	}
	for _, tc := range cases {
		got := InferCapabilities(tc.name)
		if len(got) == 0 {
			t.Errorf("InferCapabilities(%q) = empty, want a family guess", tc.name)
			continue
		}
		set := domain.NewCapabilitySet(got...)
		for _, want := range tc.want {
			if !set.Contains(want) {
				t.Errorf("InferCapabilities(%q) = %v, missing %q", tc.name, got, want)
			}
		}
	}
}

// Non-chat models must never be dressed as chat: an image model marked
// tools would route it text traffic it cannot serve.
func TestInferCapabilitiesSkipsNonChat(t *testing.T) {
	for _, name := range []string{
		"gpt-image-2", "kling-v2-6", "nano-banana-pro", "dall-e-3",
		"veo-3", "whisper-large", "text-embed-ada",
	} {
		if got := InferCapabilities(name); len(got) != 0 {
			t.Errorf("InferCapabilities(%q) = %v, want nil for a non-chat model", name, got)
		}
	}
}

// Unknown names stay unknown: empty means "use the kind default", which a
// probe or an edit can resolve later.
func TestInferCapabilitiesUnknownStaysEmpty(t *testing.T) {
	for _, name := range []string{"", "agnes-2.5-flash", "m1", "my-finetune-7b"} {
		if got := InferCapabilities(name); len(got) != 0 {
			t.Errorf("InferCapabilities(%q) = %v, want nil", name, got)
		}
	}
}

// An ids-only listing still creates capable rows for known families: zero
// upstream cost, no probing involved.
func TestSyncModelsInfersCapabilitiesOnCreate(t *testing.T) {
	adapter := &fakeAdapter{name: "acme", models: []string{"claude-fable-5-1", "gpt-image-2", "mystery-9"}}
	store := &memModelStore{}

	res, err := SyncModels(context.Background(), adapter, store, SyncOptions{
		ProviderID: "p1", Environment: domain.EnvTest,
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Created) != 3 {
		t.Fatalf("created = %v", res.Created)
	}
	if res.CapabilitiesInferred != 1 {
		t.Errorf("inferred = %d, want 1 (only the claude model is guessable)", res.CapabilitiesInferred)
	}
	if res.CapabilitySource != CapabilitySourceInferred {
		t.Errorf("source = %q, want %q", res.CapabilitySource, CapabilitySourceInferred)
	}
	byName := map[string]domain.Model{}
	for _, m := range store.rows {
		byName[m.Name] = m
	}
	claudeRow := byName["claude-fable-5-1"]
	if !claudeRow.CapabilitySet().Contains(domain.CapTools) {
		t.Errorf("claude row has no tools: %v", claudeRow.Capabilities)
	}
	if got := claudeRow.Metadata[CapabilitiesSourceKey]; got != CapabilitiesSourceInferred {
		t.Errorf("source = %q, want %q", got, CapabilitiesSourceInferred)
	}
	// Image and unknown models arrive exactly as before: silent.
	imageRow := byName["gpt-image-2"]
	mysteryRow := byName["mystery-9"]
	if len(imageRow.Capabilities) != 0 || len(mysteryRow.Capabilities) != 0 {
		t.Errorf("non-guessable rows must stay empty: image=%v mystery=%v",
			imageRow.Capabilities, mysteryRow.Capabilities)
	}
}

// The backlog heals on re-sync: existing silent rows of known families are
// filled without any probe traffic.
func TestSyncModelsInfersCapabilitiesOnExistingRows(t *testing.T) {
	adapter := &fakeAdapter{name: "acme", models: []string{"claude-fable-5-1"}}
	store := &memModelStore{rows: []domain.Model{
		{ID: "x", ProviderID: "p1", Name: "claude-fable-5-1"},
	}}

	res, err := SyncModels(context.Background(), adapter, store, SyncOptions{ProviderID: "p1"})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.CapabilitiesInferred != 1 {
		t.Fatalf("inferred = %d, want 1", res.CapabilitiesInferred)
	}
	row := store.rows[len(store.rows)-1]
	if !row.CapabilitySet().Contains(domain.CapTools) {
		t.Fatalf("existing row was not inferred: %v", row.Capabilities)
	}
}

// Probing never spends tokens on models that are not chat completions, even
// when asked explicitly: an image endpoint billed for a chat probe is pure
// waste, and an accepted shape would mislabel the row.
func TestDetectSkipsNonChatModels(t *testing.T) {
	stub := &probeStub{succeed: map[domain.Capability]bool{domain.CapTools: true}}
	store := &memModelStore{rows: []domain.Model{
		{ID: "x", ProviderID: "p1", Name: "gpt-image-2", Status: domain.ModelActive},
		{ID: "y", ProviderID: "p1", Name: "m1", Status: domain.ModelActive},
	}}
	report, err := DetectMissingCapabilities(context.Background(), stub, store,
		domain.Provider{ID: "p1", Name: "probe-stub"}, DetectionOptions{
			Probes: []domain.Capability{domain.CapTools},
		})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	for _, name := range report.Attempted {
		if name == "gpt-image-2" {
			t.Fatalf("image model was probed: %v", report.Attempted)
		}
	}
	if len(report.Attempted) != 1 {
		t.Fatalf("attempted = %v, want only the chat-unknown model", report.Attempted)
	}
}

// Trust order: published catalogue metadata replaces an earlier guess, but
// never a declared or probed list.
func TestSyncModelsCatalogueUpgradesInference(t *testing.T) {
	adapter := richAdapter{
		fakeAdapter: &fakeAdapter{name: "acme", models: []string{"m1"}},
		models: []providers.RemoteModel{{
			Name:         "m1",
			Capabilities: []domain.Capability{domain.CapChat, domain.CapVision},
		}},
	}
	store := &memModelStore{rows: []domain.Model{{
		ID: "x", ProviderID: "p1", Name: "m1",
		Capabilities: []domain.Capability{domain.CapChat, domain.CapTools},
		Metadata:     map[string]string{CapabilitiesSourceKey: CapabilitiesSourceInferred},
	}}}

	res, err := SyncModels(context.Background(), adapter, store, SyncOptions{ProviderID: "p1"})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.CapabilitiesFilled != 1 {
		t.Fatalf("filled = %d, want 1: published metadata outranks a guess", res.CapabilitiesFilled)
	}
	row := store.rows[len(store.rows)-1]
	if !row.CapabilitySet().Contains(domain.CapVision) {
		t.Errorf("guess was not upgraded by the catalogue: %v", row.Capabilities)
	}
	if got := row.Metadata[CapabilitiesSourceKey]; got != CapabilitiesSourceProvider {
		t.Errorf("source = %q, want %q", got, CapabilitiesSourceProvider)
	}
}
