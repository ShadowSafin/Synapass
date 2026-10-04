package cache

import (
	"context"
	"strings"

	"github.com/shadowsafin/synapass/internal/domain"
)

// errKeyScopeNeedsTenant is returned when a key flush cannot address any
// entry: exact keys live under tenant namespaces, so the tenant is
// required to name them.
var errKeyScopeNeedsTenant = domain.NewError(domain.ErrCodeInvalidRequest,
	"a key-scope invalidation requires tenant_id: exact keys live under tenant namespaces")

// InvalidateScope describes one flush operation. TenantID is required for
// tenant/model/provider scopes; empty means global (admin "flush all").
type InvalidateScope struct {
	Scope    string
	TenantID string
	Model    string
	Provider string
	Key      string
	Reason   string
}

// tenantPrefix returns the Redis namespace prefix for a tenant. All cache
// bodies live under "response:<ns>" where <ns> starts with the tenant, so a
// tenant flush touches exactly that tenant's keys and nothing else.
func tenantPrefix(tenantID string) string {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return ""
	}
	return "tenant:" + tenantID + ":"
}

// Invalidate removes entries for a scope and returns the removed count.
// Tenant isolation is structural: global exact/prefix/semantic prefixes are
// never deleted for a tenant-scoped flush, only that tenant's namespace.
func (c *Cache) Invalidate(ctx context.Context, scope InvalidateScope) (int, error) {
	if c == nil {
		return 0, nil
	}
	reason := scope.Reason
	if reason == "" {
		reason = "manual_flush"
	}
	removed := 0
	switch scope.Scope {
	case "", "all":
		if c.store != nil {
			for _, p := range []string{"tenant:", "exact:", "prefix:", "semantic:"} {
				n, err := c.store.DeletePrefix(ctx, p)
				removed += n
				if err != nil {
					return removed, err
				}
			}
		}
		c.mu.Lock()
		removed += len(c.memIndex)
		c.memIndex = map[string]semanticEntry{}
		c.stats.Invalidations++
		c.mu.Unlock()
		return removed, nil
	case "tenant":
		prefix := tenantPrefix(scope.TenantID)
		if prefix == "" {
			return c.Invalidate(ctx, InvalidateScope{Scope: "all", Reason: reason})
		}
		if c.store != nil {
			n, err := c.store.DeletePrefix(ctx, "response:"+prefix)
			removed += n
			if err != nil {
				return removed, err
			}
			// Legacy un-namespaced keys cannot be attributed; leave them.
		}
		c.mu.Lock()
		for k, e := range c.memIndex {
			if e.tenant == scope.TenantID {
				delete(c.memIndex, k)
				removed++
			}
		}
		c.stats.Invalidations++
		c.mu.Unlock()
		return removed, nil
	case "key":
		// Exact keys are tenant-namespaced in the store, so a key flush
		// without a tenant cannot address any live entry: failing here
		// is honest, while deleting nothing and reporting success would
		// leave operators believing a key was flushed when it was not.
		if scope.TenantID == "" || scope.Key == "" {
			return 0, errKeyScopeNeedsTenant
		}
		// Resolve both key forms: the dashboard may name the bare
		// "exact:<hash>" or the namespaced "tenant:<id>:exact:<hash>".
		forms := map[string]bool{scope.Key: true}
		nsForm := "tenant:" + scope.TenantID + ":" + strings.TrimPrefix(scope.Key, "tenant:"+scope.TenantID+":")
		forms[nsForm] = true
		c.mu.Lock()
		var tierKeys []string
		seen := map[string]bool{}
		for f := range forms {
			if ref, ok := c.keyIndex[f]; ok {
				for _, sk := range ref.storeKeys {
					if !seen[sk] {
						seen[sk] = true
						tierKeys = append(tierKeys, sk)
					}
				}
				delete(c.keyIndex, f)
			}
		}
		// Fallback sweep for entries the reverse index no longer holds
		// (evicted under pressure): any semantic entry derived from the
		// same logical request shares its exact key.
		wantExact := strings.TrimPrefix(scope.Key, "tenant:"+scope.TenantID+":")
		for k, e := range c.memIndex {
			if e.tenant != "" && e.tenant != scope.TenantID {
				continue
			}
			if ExactKey(e.input) == wantExact {
				if !seen[e.nsKey] {
					seen[e.nsKey] = true
					tierKeys = append(tierKeys, e.nsKey)
				}
				delete(c.memIndex, k)
				removed++
			}
		}
		c.stats.Invalidations++
		c.mu.Unlock()
		if c.store != nil {
			for _, sk := range tierKeys {
				n, err := c.store.DeletePrefix(ctx, "response:"+sk)
				removed += n
				if err != nil {
					return removed, err
				}
			}
			// Legacy bare exact keys predate namespacing; delete them for
			// compatibility when the reverse index knew nothing about them.
			if len(tierKeys) == 0 {
				n, err := c.store.DeletePrefix(ctx, "response:"+wantExact)
				removed += n
				if err != nil {
					return removed, err
				}
			}
		}
		return removed, nil
	case "model", "provider":
		// Model/provider changes cannot be mapped to key hashes without an
		// index, so the safe action is a tenant-scoped flush when a tenant is
		// given, otherwise a global flush. The reason records what changed so
		// the audit trail stays honest about the blast radius.
		if scope.TenantID != "" {
			return c.Invalidate(ctx, InvalidateScope{Scope: "tenant", TenantID: scope.TenantID, Reason: reason})
		}
		return c.Invalidate(ctx, InvalidateScope{Scope: "all", Reason: reason})
	default:
		return c.Invalidate(ctx, InvalidateScope{Scope: "all", Reason: reason})
	}
}
