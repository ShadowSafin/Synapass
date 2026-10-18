package api

import (
	"net/http"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

func TestAdminScopeForUsesWholeSegments(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   domain.Scope
	}{
		// The mis-grant that motivated the segment rewrite: tool policy is not
		// routing policy, and a policy admin must not be able to change what
		// the gateway may execute.
		{http.MethodPut, "/admin/v1/tool-policies", domain.ScopeAdminProviders},
		{http.MethodDelete, "/admin/v1/tool-policies/tp-1", domain.ScopeAdminProviders},
		{http.MethodGet, "/admin/v1/tool-policies", domain.ScopeReadUsage},

		// Cache policy stays under the policy scope, but invalidation is
		// platform-wide and requires full admin.
		{http.MethodPut, "/admin/v1/cache/policies", domain.ScopeAdminPolicies},
		{http.MethodDelete, "/admin/v1/cache/policies/cp-1", domain.ScopeAdminPolicies},
		{http.MethodPost, "/admin/v1/cache/invalidate", domain.ScopeAdminAll},
		{http.MethodGet, "/admin/v1/cache/stats", domain.ScopeReadUsage},

		{http.MethodPost, "/admin/v1/keys", domain.ScopeAdminKeys},
		{http.MethodPatch, "/admin/v1/keys/k-1", domain.ScopeAdminKeys},
		{http.MethodPost, "/admin/v1/keys/k-1/rotate", domain.ScopeAdminKeys},
		{http.MethodDelete, "/admin/v1/keys/k-1", domain.ScopeAdminKeys},
		{http.MethodGet, "/admin/v1/keys", domain.ScopeAdminKeys},

		{http.MethodPost, "/admin/v1/tenants", domain.ScopeAdminTenants},
		{http.MethodGet, "/admin/v1/tenants", domain.ScopeAdminTenants},

		{http.MethodPost, "/admin/v1/providers", domain.ScopeAdminProviders},
		{http.MethodPut, "/admin/v1/providers/p-1/credential", domain.ScopeAdminProviders},
		{http.MethodGet, "/admin/v1/providers", domain.ScopeAdminProviders},
		{http.MethodPost, "/admin/v1/models", domain.ScopeAdminProviders},
		{http.MethodGet, "/admin/v1/models", domain.ScopeReadUsage},

		// Tunnel mutations can expose the console publicly: full admin only.
		{http.MethodPost, "/admin/v1/tunnels/create", domain.ScopeAdminAll},
		{http.MethodGet, "/admin/v1/tunnels/status", domain.ScopeReadUsage},

		// A prefix that merely contains a protected word must not inherit its
		// scope; it falls through to the restrictive default.
		{http.MethodPost, "/admin/v1/policies-export", domain.ScopeAdminAll},
		{http.MethodPost, "/admin/v1/not-policies", domain.ScopeAdminAll},

		// Unmatched reads stay readable with the read scope, unmatched writes
		// require everything.
		{http.MethodGet, "/admin/v1/benchmark/jobs", domain.ScopeReadUsage},
		{http.MethodPost, "/admin/v1/benchmark/run", domain.ScopeAdminAll},
	}

	for _, tc := range cases {
		if got := adminScopeFor(tc.method, tc.path); got != tc.want {
			t.Errorf("adminScopeFor(%s %s) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestAdminPathSegmentsHandlesMountRoot(t *testing.T) {
	if first, second := adminPathSegments("/admin/v1"); first != "" || second != "" {
		t.Fatalf("segments = %q, %q, want empty", first, second)
	}
	if first, second := adminPathSegments("/admin/v1/keys/k-1"); first != "keys" || second != "k-1" {
		t.Fatalf("segments = %q, %q", first, second)
	}
	if first, _ := adminPathSegments("/v1/chat/completions"); first != "" {
		t.Fatalf("a non-admin path yielded segment %q", first)
	}
}
