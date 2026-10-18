package api

import (
	"context"
	"testing"

	"github.com/shadowsafin/synapass/internal/auth"
	"github.com/shadowsafin/synapass/internal/domain"
)

// scopeContext builds a request context carrying a principal with the given
// scopes, the way authMiddleware does after a successful authentication.
func scopeContext(scopes ...string) context.Context {
	p := &auth.Principal{
		Tenant: &domain.Tenant{ID: "t-1"},
		APIKey: &domain.APIKey{ID: "k-1", TenantID: "t-1", Scopes: scopes},
	}
	return context.WithValue(context.Background(), ctxKeyPrincipal, p)
}

func systemContext() context.Context {
	return context.WithValue(context.Background(), ctxKeyPrincipal,
		&auth.Principal{System: true, Tenant: &domain.Tenant{ID: "t-1"}})
}

func TestNormalizeGrantedScopesRejectsEscalation(t *testing.T) {
	ctx := scopeContext(string(domain.ScopeAdminKeys))

	if _, err := normalizeGrantedScopes(ctx, []string{string(domain.ScopeAdminAll)}); err == nil {
		t.Fatal("a keys:admin key must not be able to grant '*'")
	}
	if _, err := normalizeGrantedScopes(ctx, []string{string(domain.ScopeAdminProviders)}); err == nil {
		t.Fatal("a keys:admin key must not grant an unheld scope")
	}
	// The default set (inference) is also a grant, and is refused for the same
	// reason: this caller holds no inference scope to hand out.
	if _, err := normalizeGrantedScopes(ctx, nil); err == nil {
		t.Fatal("the default inference grant must be confined like any other grant")
	}
}

func TestNormalizeGrantedScopesAllowsHeldScopes(t *testing.T) {
	ctx := scopeContext(string(domain.ScopeAdminKeys), string(domain.ScopeInference))

	scopes, err := normalizeGrantedScopes(ctx, []string{"inference", " inference ", "keys:admin"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(scopes) != 2 {
		t.Fatalf("scopes = %v, want the deduplicated pair", scopes)
	}
}

func TestNormalizeGrantedScopesDefaultsToInference(t *testing.T) {
	scopes, err := normalizeGrantedScopes(systemContext(), nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != string(domain.ScopeInference) {
		t.Fatalf("scopes = %v, want [inference]", scopes)
	}
}

func TestNormalizeGrantedScopesRejectsUnknownScope(t *testing.T) {
	if _, err := normalizeGrantedScopes(systemContext(), []string{"root"}); err == nil {
		t.Fatal("an unknown scope must be rejected")
	}
}

func TestNormalizeGrantedScopesRequiresPrincipal(t *testing.T) {
	if _, err := normalizeGrantedScopes(context.Background(), []string{"inference"}); err == nil {
		t.Fatal("an unauthenticated context must not grant scopes")
	}
}

func TestRequireKeyWithinCallerBlocksRotatingAPrivilegedKey(t *testing.T) {
	ctx := scopeContext(string(domain.ScopeAdminKeys))
	privileged := &domain.APIKey{ID: "k-2", Scopes: []string{string(domain.ScopeAdminAll)}}

	if err := requireKeyWithinCaller(ctx, privileged); err == nil {
		t.Fatal("rotation/update of a '*' key must be refused to a narrow key admin")
	}
	if err := requireKeyWithinCaller(systemContext(), privileged); err != nil {
		t.Fatalf("the bootstrap key must manage any key: %v", err)
	}
}
