package cache

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// semanticIndexPrefix namespaces the durable sidecars for the semantic
// similarity index. Every semantic body written to the store gets a small
// companion record under sidx:<nsSem> carrying the embedding and the serving
// context needed to rebuild the local search index after a restart. The
// sidecar shares the body's TTL so the two expire together.
const semanticIndexPrefix = "sidx:"

// semanticIndexKey maps a semantic body key to its durable sidecar.
func semanticIndexKey(nsSem string) string {
	if strings.HasPrefix(nsSem, semanticIndexPrefix) {
		return nsSem
	}
	return semanticIndexPrefix + nsSem
}

// semanticRecord is the durable form of one similarity-index entry. It
// carries everything Warmup needs to repopulate memIndex without
// re-deriving anything: the embedding, the full key input for serving
// validation, and the expiry that keeps rebuilds honest. The response body
// itself stays under the semantic body key and is re-read on warmup, so the
// sidecar stays small.
type semanticRecord struct {
	NSKey     string             `json:"ns_key"`
	Key       string             `json:"key"`
	Tenant    string             `json:"tenant"`
	Embedding map[string]float64 `json:"embedding"`
	Input     KeyInput           `json:"input"`
	StoredAt  time.Time          `json:"stored_at"`
	ExpiresAt time.Time          `json:"expires_at"`
}

// Scanner is the optional enumeration capability a Store may provide. The
// cache only needs it on the Warmup path, so stores that cannot enumerate
// (plain maps in unit tests) simply omit it and Warmup becomes a no-op
// rather than a startup failure.
type Scanner interface {
	Scan(ctx context.Context, pattern string) ([]string, error)
}

// Warmup repopulates the local semantic search index from the durable
// sidecars in the store, so a restart does not cold-start semantic reuse.
//
// Behaviour: SCANs for sidx:* sidecars, skips expired or corrupt records
// and entries whose body is already gone, and inserts the survivors ordered
// oldest-first bounded by MaxSemanticEntries (newest win, matching the
// oldest-first eviction on the write path). Tenant isolation is preserved
// because each record carries its tenant and lookups still filter on it;
// Warmup itself loads every tenant.
//
// Warmup never fails when the store cannot enumerate: a store without Scan
// yields zero entries and a nil error. A Redis outage likewise surfaces as
// an error the caller may log and ignore — the index simply rebuilds from
// new stores.
func (c *Cache) Warmup(ctx context.Context) (int, error) {
	if c == nil || c.store == nil || !c.opts.SemanticEnabled {
		return 0, nil
	}
	scanner, ok := c.store.(Scanner)
	if !ok {
		return 0, nil
	}
	keys, err := scanner.Scan(ctx, semanticIndexPrefix+"*")
	if err != nil {
		return 0, err
	}
	if len(keys) == 0 {
		return 0, nil
	}
	now := time.Now()
	type candidate struct {
		rec  semanticRecord
		body []byte
	}
	candidates := make([]candidate, 0, len(keys))
	for _, k := range keys {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		raw, found, err := c.get(ctx, k)
		if err != nil || !found || len(raw) == 0 {
			continue
		}
		var rec semanticRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		if rec.NSKey == "" || len(rec.Embedding) == 0 {
			continue
		}
		if !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) {
			continue
		}
		body, found, err := c.get(ctx, rec.NSKey)
		if err != nil || !found || len(body) == 0 {
			// The body expired or was evicted without its sidecar:
			// indexing a dangling pointer would serve nothing.
			continue
		}
		if rec.StoredAt.IsZero() {
			rec.StoredAt = rec.ExpiresAt
		}
		candidates = append(candidates, candidate{rec: rec, body: body})
	}
	// Oldest first so the bound below keeps the newest entries, mirroring
	// the oldest-first eviction on the write path.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].rec.StoredAt.Before(candidates[j].rec.StoredAt)
	})
	max := c.opts.MaxSemanticEntries
	if max <= 0 {
		max = 2000
	}
	if len(candidates) > max {
		candidates = candidates[len(candidates)-max:]
	}
	c.mu.Lock()
	for _, cand := range candidates {
		if c.memIndex == nil {
			c.memIndex = map[string]semanticEntry{}
		}
		c.memIndex[cand.rec.NSKey] = semanticEntry{
			key:       cand.rec.Key,
			nsKey:     cand.rec.NSKey,
			embedding: cand.rec.Embedding,
			body:      cand.body,
			expiresAt: cand.rec.ExpiresAt,
			storedAt:  cand.rec.StoredAt,
			tenant:    cand.rec.Tenant,
			input:     cand.rec.Input,
		}
	}
	c.evictSemanticLocked()
	c.mu.Unlock()
	return len(candidates), nil
}

// StartWarmup rebuilds the semantic index in the background. It never blocks
// the caller and never fails startup: errors are logged and the cache
// continues with whatever index it has (cold or partial) until new stores
// repopulate it.
func (c *Cache) StartWarmup(ctx context.Context, logger *slog.Logger) {
	if c == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := c.Warmup(wctx)
		if err != nil {
			logger.Warn("semantic cache warmup failed; continuing with a cold index", "error", err)
			return
		}
		if n > 0 {
			logger.Info("semantic cache warmed", "entries", n)
		}
	}()
}
