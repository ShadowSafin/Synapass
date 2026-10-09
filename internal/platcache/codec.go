package platcache

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// codecVersion lets the envelope shape change without a manual flush:
// entries with an unexpected version are discarded.
const codecVersion = 1

// compressThreshold avoids paying gzip for small metadata payloads.
const compressThreshold = 4 << 10

// envelope is the L2 wire form: versioned, checksummed, optionally
// compressed JSON. StoredAt/ExpiresAt/SoftExpiresAt drive SWR.
type envelope struct {
	V        int             `json:"v"`
	Kind     string          `json:"kind"`
	Key      string          `json:"key"`
	Tenant   string          `json:"tenant,omitempty"`
	Data     json.RawMessage `json:"data"`
	StoredAt time.Time       `json:"stored_at"`
	// SoftExpiresAt: serve stale + refresh in background after this.
	SoftExpiresAt time.Time `json:"soft_expires_at"`
	// ExpiresAt: absolute expiry; never serve after this.
	ExpiresAt time.Time `json:"expires_at"`
	// Compressed marks gzip(Data).
	Compressed bool `json:"compressed,omitempty"`
	// Sum is sha256 hex of the (uncompressed) Data for corruption checks.
	Sum string `json:"sum"`
}

func encodeEnvelope(kind Kind, key, tenant string, value any, hard, soft time.Duration) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("platcache: encode value: %w", err)
	}
	now := time.Now()
	sum := sha256.Sum256(raw)
	data := raw
	compressed := false
	if len(raw) >= compressThreshold {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(raw); err == nil {
			if err := gz.Close(); err == nil && buf.Len() < len(raw) {
				data = buf.Bytes()
				compressed = true
			}
		}
	}
	env := envelope{
		V:             codecVersion,
		Kind:          string(kind),
		Key:           key,
		Tenant:        tenant,
		Data:          data,
		StoredAt:      now,
		SoftExpiresAt: now.Add(soft),
		ExpiresAt:     now.Add(hard),
		Compressed:    compressed,
		Sum:           hex.EncodeToString(sum[:]),
	}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("platcache: encode envelope: %w", err)
	}
	return out, nil
}

// decodeEnvelopeInto verifies version, expiry shape, checksum and tenant
// binding, then unmarshals Data into dst. It reports soft/hard staleness
// so callers can decide between fresh hit, stale hit, or miss.
func decodeEnvelopeInto(raw []byte, key, tenant string, dst any) (softStale, hardExpired bool, storedAt time.Time, err error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return false, false, time.Time{}, fmt.Errorf("platcache: decode envelope: %w", err)
	}
	if env.V != codecVersion {
		return false, false, time.Time{}, fmt.Errorf("platcache: envelope version %d != %d", env.V, codecVersion)
	}
	if len(env.Data) == 0 {
		return false, false, time.Time{}, fmt.Errorf("platcache: empty payload")
	}
	if env.Key != "" && key != "" && env.Key != key {
		return false, false, time.Time{}, fmt.Errorf("platcache: key mismatch")
	}
	// Tenant binding: a tenant-scoped entry never serves another tenant,
	// even if the keyspace ever collides.
	if tenant != "" && env.Tenant != "" && env.Tenant != tenant {
		return false, false, time.Time{}, fmt.Errorf("platcache: tenant mismatch")
	}
	data := []byte(env.Data)
	if env.Compressed {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return false, false, time.Time{}, fmt.Errorf("platcache: gunzip: %w", err)
		}
		data, err = io.ReadAll(gz)
		_ = gz.Close()
		if err != nil {
			return false, false, time.Time{}, fmt.Errorf("platcache: gunzip read: %w", err)
		}
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != env.Sum {
		return false, false, time.Time{}, fmt.Errorf("platcache: checksum mismatch")
	}
	if dst != nil {
		if err := json.Unmarshal(data, dst); err != nil {
			return false, false, time.Time{}, fmt.Errorf("platcache: decode value: %w", err)
		}
	}
	now := time.Now()
	if !env.ExpiresAt.IsZero() && now.After(env.ExpiresAt) {
		return true, true, env.StoredAt, nil
	}
	if !env.SoftExpiresAt.IsZero() && now.After(env.SoftExpiresAt) {
		return true, false, env.StoredAt, nil
	}
	return false, false, env.StoredAt, nil
}
