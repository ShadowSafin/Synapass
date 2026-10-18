package providers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/netguard"
)

// Options configures adapter construction. All fields are optional; the zero
// value produces a fully functional adapter with a tuned default transport.
type Options struct {
	// Client overrides the HTTP client. Tests inject a stub transport here.
	Client HTTPDoer
	// Logger receives adapter-level diagnostics. Chunks and bodies are never
	// logged at info level regardless of this setting.
	Logger *slog.Logger
	// Timeouts supplies connect and response budgets.
	Timeouts domain.TimeoutPolicy
	// UserAgent identifies the gateway to upstreams. Some providers log it and
	// it is the first thing to check when a provider reports unexpected traffic.
	UserAgent string
	// MaxResponseBytes caps a buffered response body.
	MaxResponseBytes int64
	// MaxErrorBodyBytes caps how much of an error body is read for diagnostics.
	MaxErrorBodyBytes int64
}

func (o Options) withDefaults() Options {
	if o.UserAgent == "" {
		o.UserAgent = "Synapass/1.0 (+https://github.com/shadowsafin/synapass)"
	}
	if o.MaxResponseBytes <= 0 {
		// 32 MiB comfortably fits a large completion with many choices while
		// still bounding memory per in-flight request.
		o.MaxResponseBytes = 32 << 20
	}
	if o.MaxErrorBodyBytes <= 0 {
		o.MaxErrorBodyBytes = 64 << 10
	}
	if o.Timeouts.PerAttempt <= 0 {
		o.Timeouts = o.Timeouts.Normalize()
	}
	return o
}

// NewHTTPClient builds the shared HTTP client used by every adapter.
//
// One client is shared across providers because the connection pool is keyed by
// host internally; giving each provider its own client would prevent connection
// reuse and multiply open sockets. The settings are tuned for a gateway that
// talks to a handful of hosts with high concurrency.
func NewHTTPClient(timeouts domain.TimeoutPolicy) *http.Client {
	timeouts = timeouts.Normalize()

	dialer := &net.Dialer{
		Timeout:   timeouts.Connect,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// The dialer resolves once and connects only to an address that passed
		// the provider policy. Loopback and private runtimes remain reachable;
		// link-local addresses (cloud instance metadata) never are, even if the
		// provider's hostname is re-pointed after it was validated.
		DialContext:           netguard.DialContext(dialer, netguard.IsBlockedProviderIP),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       0, // unlimited; provider concurrency is capped by policy
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeouts.Connect,
		ExpectContinueTimeout: 1 * time.Second,
		// ResponseHeaderTimeout is deliberately NOT set.
		//
		// It used to be `timeouts.FirstToken` (30s by default), which is
		// correct for a streaming request — headers arrive immediately and
		// the body follows — but wrong for a buffered (stream:false) request.
		// A provider only sends response headers once it has generated the
		// whole answer, so a long completion routinely exceeded 30s *before a
		// single header arrived* and the transport aborted a healthy request
		// with "provider timed out".
		//
		// Time-to-first-byte is therefore enforced per attempt by the request
		// context (see attemptContext in the routing executor), which covers
		// connect + headers + body and is configurable per policy. The
		// transport stays free of a whole-response deadline so it can never
		// silently truncate a large completion.
		// Default TLS configuration; certificate verification stays enabled.
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	return &http.Client{
		Transport: transport,
		// No Client.Timeout: it would apply to the whole body read and break
		// streaming. Per-attempt deadlines come from the request context.
	}
}

// buildURL joins the provider base URL with a path, tolerating a trailing slash
// on the base and a leading slash on the path without producing a double slash.
func buildURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if path == "" {
		return base
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// resolveBaseURL validates and normalizes a provider base URL.
func resolveBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("provider base URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("provider base URL %q is invalid: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("provider base URL %q must use http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("provider base URL %q is missing a host", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// applyAuth attaches the configured credentials to a request.
//
// Credentials are resolved once at construction time and stored as a []byte
// rather than read from the environment per request. That keeps the request path
// free of os.Getenv (which takes a lock) and means rotating an env var requires a
// reload, which is the safer semantic: a partially-rotated fleet cannot happen by
// accident.
func applyAuth(req *http.Request, p domain.Provider, credential string) {
	style := p.AuthStyle
	if style == "" {
		// An unset style is resolved from the provider kind, using the single
		// mapping the persistence layer also applies.
		style = domain.DefaultAuthStyle(p.Kind)
	}

	switch style {
	case domain.AuthNone:
		// Local runtimes typically accept an empty credential. Sending a bogus
		// Authorization header is worse than sending none.
		return
	case domain.AuthHeader:
		header := p.HeaderName
		if header == "" {
			header = domain.DefaultAuthHeader(p.Kind)
		}
		if header == "" {
			header = "x-api-key"
		}
		if credential != "" {
			req.Header.Set(header, credential)
		}
	default:
		if credential != "" {
			// Setting the header directly avoids allocation in Header.Set's
			// canonicalization path for high-volume inference traffic.
			req.Header.Set("Authorization", "Bearer "+credential)
		}
	}
}

// encodeJSON marshals a body, returning a normalized error on failure. A marshal
// failure is always a gateway bug, so it is reported as internal.
func encodeJSON(v any) ([]byte, error) {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	// Providers reject HTML-escaped payloads in some cases; disabling escaping
	// keeps the wire format predictable and the body smaller.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "failed to encode provider request").
			Wrap(err)
	}
	return buf.Bytes(), nil
}

// decodeJSON decodes a buffered body.
func decodeJSON(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	return dec.Decode(v)
}

// contextWithAttempt returns a context bounded by the smaller of the caller's
// deadline and the per-attempt budget, so a single slow provider cannot consume
// the whole request budget.
func contextWithAttempt(ctx context.Context, perAttempt time.Duration) (context.Context, context.CancelFunc) {
	if perAttempt <= 0 {
		return context.WithCancel(ctx)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if time.Until(deadline) <= perAttempt {
			// The caller's budget is already tighter; adding another timer
			// would only add a wakeup.
			return context.WithCancel(ctx)
		}
	}
	return context.WithTimeout(ctx, perAttempt)
}

// readLimited reads at most limit bytes, returning a distinguishable error when
// the cap is hit so the caller can report a truncated body rather than a parse
// failure.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	buf := &bytes.Buffer{}
	// Read one extra byte so an over-limit body is detectable.
	n, err := io.Copy(buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("response body exceeded the %d byte limit", limit)
	}
	return buf.Bytes(), nil
}
