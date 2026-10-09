package platcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// HTTP cache policy for safe GET endpoints. Only public or non-sensitive
// endpoints with explicit opt-in are cached; everything else bypasses.

// SafeGET reports whether a GET endpoint is eligible for HTTP response
// caching. The allowlist is explicit: exact path prefixes the operator
// marks public. Authenticated, tenant-specific, or sensitive paths never
// qualify unless the caller passes an explicit scope allowlist.
func SafeGET(method, path string, publicPrefixes []string) bool {
	if method != http.MethodGet {
		return false
	}
	lower := strings.ToLower(path)
	// Hard denylists first: never cache these regardless of allowlist.
	for _, bad := range []string{
		"/admin", "/keys", "/auth", "/login", "/setup", "/logout",
		"/credential", "/token", "/secret", "/payment", "/billing",
		"/feedback", "/requests/", "/tunnel",
	} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	for _, p := range publicPrefixes {
		if p == "" {
			continue
		}
		if strings.HasPrefix(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// DefaultPublicGETPrefixes are the safe GET endpoints cached by default:
// version, health, public model lists. Everything else requires explicit
// opt-in via Options or call-site prefixes.
func DefaultPublicGETPrefixes() []string {
	return []string{"/version", "/healthz", "/health", "/v1/models"}
}

// HashGETPath deterministically hashes method+path+query for the key.
func HashGETPath(method, path, query string) string {
	sum := sha256.Sum256([]byte(method + "\x00" + path + "\x00" + query))
	return hex.EncodeToString(sum[:])[:32]
}

// GetHTTPResponse reads a cached safe-GET response body.
func (c *Cache) GetHTTPResponse(ctx context.Context, scope, pathHash string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	key := HTTPResponseKey(scope, pathHash)
	ns := c.namespaced(key)
	if c.l1 != nil {
		if raw, ok := c.l1.Get(ns); ok {
			var body []byte
			if soft, hard, _, err := decodeEnvelopeInto(raw, key, "", &body); err == nil && !hard {
				atomicAdd(&c.stats.Hits, 1)
				if soft {
					atomicAdd(&c.stats.StaleHits, 1)
				}
				return body, true
			}
			c.l1.Delete(ns)
		}
	}
	if c.l2 == nil {
		return nil, false
	}
	raw, ok, err := c.l2.Get(ctx, ns)
	if err != nil || !ok {
		return nil, false
	}
	var body []byte
	if soft, hard, _, err := decodeEnvelopeInto(raw, key, "", &body); err == nil && !hard {
		if c.l1 != nil {
			c.l1.Set(ns, raw, L1TTL(KindHTTPResponse))
		}
		atomicAdd(&c.stats.Hits, 1)
		if soft {
			atomicAdd(&c.stats.StaleHits, 1)
		}
		return body, true
	}
	return nil, false
}

// SetHTTPResponse caches a safe-GET response body with strict control.
// Callers must have checked SafeGET first; this is enforced again here
// against the default public prefixes plus extraPrefixes.
func (c *Cache) SetHTTPResponse(ctx context.Context, method, path, query, scope string, body []byte, ttl time.Duration, extraPrefixes ...string) error {
	if c == nil || len(body) == 0 {
		return nil
	}
	prefixes := append(DefaultPublicGETPrefixes(), extraPrefixes...)
	if !SafeGET(method, path, prefixes) {
		return nil // not eligible: silent no-op, never an error.
	}
	if ttl <= 0 {
		hard, _ := TTLFor(KindHTTPResponse)
		ttl = hard
	}
	key := HTTPResponseKey(scope, HashGETPath(method, path, query))
	hard, soft := ttl, ttl/2
	raw, err := encodeEnvelope(KindHTTPResponse, key, "", body, hard, soft)
	if err != nil {
		return nil
	}
	ns := c.namespaced(key)
	if c.l1 != nil {
		c.l1.Set(ns, raw, L1TTL(KindHTTPResponse))
	}
	if c.l2 != nil {
		_ = c.l2.Set(ctx, ns, raw, hard+10*time.Second)
	}
	return nil
}

// InvalidateHTTPResponses drops cached GET responses for a scope.
func (c *Cache) InvalidateHTTPResponses(ctx context.Context, scope string) (int, error) {
	if c == nil {
		return 0, nil
	}
	if strings.TrimSpace(scope) == "" {
		return c.InvalidateByPrefix(ctx, "http:get:")
	}
	n, _ := c.InvalidateByPrefix(ctx, "http:get:"+normSegment(scope)+":")
	return n, nil
}
