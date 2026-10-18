package api

import (
	"context"
	"strings"

	adminsvc "github.com/shadowsafin/synapass/internal/admin"
	"github.com/shadowsafin/synapass/internal/domain"
)

// normalizeGrantedScopes validates a requested scope set for a key write and
// enforces the delegation rule: a key may only grant scopes it holds itself.
//
// Validation alone (adminsvc.NormalizeKeyScopes) answers "is this a scope?",
// which is not the same question as "may this caller hand it out". Without the
// second check, a key holding only keys:admin can mint a key with "*" and then
// use that key for every other administrative surface — the least-privilege
// scope model would be decorative.
//
// The bootstrap admin key is principal.System and is therefore unconstrained;
// it is how a fresh installation creates the first delegated keys.
func normalizeGrantedScopes(ctx context.Context, requested []string) ([]string, error) {
	scopes, err := adminsvc.NormalizeKeyScopes(requested)
	if err != nil {
		return nil, err
	}
	if err := requireScopesHeld(ctx, scopes); err != nil {
		return nil, err
	}
	return scopes, nil
}

// requireScopesHeld reports an error naming every scope the caller lacks.
//
// Naming them (rather than a generic denial) is deliberate: an operator who
// delegated keys:admin without the scopes it needs to delegate has to be told
// exactly what to add, or the failure reads as a bug.
func requireScopesHeld(ctx context.Context, scopes []string) error {
	p := principal(ctx)
	if p == nil {
		return domain.NewError(domain.ErrCodePermission,
			"an authenticated principal is required to write api keys")
	}
	if p.System {
		return nil
	}
	missing := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if !p.Has(domain.Scope(scope)) {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return domain.Errorf(domain.ErrCodePermission,
			"a key cannot grant scopes its creator does not hold: %s", strings.Join(missing, ", "))
	}
	return nil
}

// requireKeyWithinCaller refuses to update or rotate a key that holds scopes the
// caller does not hold.
//
// Rotation is the sharp edge: it returns the plaintext of the target key, so
// without this check a keys:admin key could rotate the platform's "*" key and
// receive its secret. Patching is included for the same reason — a name and
// expiry are trivial, but scopes are checked separately and the containment
// rule keeps the two operations consistent.
func requireKeyWithinCaller(ctx context.Context, key *domain.APIKey) error {
	if key == nil {
		return nil
	}
	return requireScopesHeld(ctx, key.Scopes)
}
