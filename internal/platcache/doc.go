// Package platcache is Synapass's production multi-layer platform cache.
//
// It sits beside the inference response cache (internal/cache) and covers
// everything else the hot paths read on every request: tenant metadata,
// workspace metadata, route mappings, provider/model catalog snapshots,
// feature flags, tenant settings, aggregated usage counters, dependency
// health, and host/path -> tenant resolution.
//
// Layers:
//
//	L1  in-process short-lived memory (ultra-fast, very short TTL)
//	L2  Redis distributed cache (shared across workers/instances)
//	L3  optional HTTP response cache for safe GET endpoints
//
// Safety rules (enforced, not advisory):
//
//   - Never cache secrets: API keys, tokens, passwords, session secrets,
//     payment secrets, raw request bodies, raw prompts, webhook payloads,
//     or anything marked private.
//   - Every tenant-scoped entry is tenant-aware in its key.
//   - Keys are namespaced and versioned so a schema change can retire a
//     whole generation without a manual flush.
//   - A Redis outage degrades to DB fallback; it never fails the request.
//   - Serialization failures bypass the cache for that request.
package platcache
