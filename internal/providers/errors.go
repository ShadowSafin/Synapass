package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// NormalizeHTTPError converts an upstream HTTP failure into a domain.Error.
//
// The mapping is status-driven first because HTTP status semantics are universal,
// then refined by the provider's own error code when it supplies one. That order
// matters: a provider returning 429 is rate limited regardless of how its body
// labels the condition, and a 200 with an error body (which some OpenAI-compatible
// servers produce) is handled by the caller, not here.
func NormalizeHTTPError(provider, model string, attempt, status int, body []byte, header http.Header) *domain.Error {
	message, vendorCode, vendorType := parseErrorBody(body)
	message = sanitizeProviderMessage(message)

	code := codeForStatus(status, vendorCode, vendorType)
	if message == "" {
		message = defaultMessageFor(status, provider)
	}

	err := domain.NewError(code, message).
		WithStatus(status).
		WithProvider(provider, model, attempt)

	// Vendor type and code are preserved on the error for logs, but the
	// normalized code remains authoritative for routing decisions.
	if vendorCode != "" {
		err.Type = vendorType
	}

	// Honour Retry-After when the provider supplies it; the orchestrator decides
	// whether to actually wait.
	if ra := parseRetryAfter(header.Get("Retry-After")); ra > 0 {
		err.Cause = &RetryAfterError{After: ra, Cause: err.Cause}
	}

	// Some OpenAI-compatible servers signal context overflow in the message
	// rather than the status, because they return 400 for every bad request.
	if code == domain.ErrCodeInvalidRequest && looksLikeContextOverflow(message) {
		err.Code = domain.ErrCodeContextLength
		err.Retryable = false
		err.FallbackEligible = true
	}
	return err
}

// RetryAfterError wraps a normalized error with the provider's requested delay.
// It is a distinct type so backoff logic can use errors.As without string
// matching or an extra field on every error.
type RetryAfterError struct {
	// After is the provider-requested delay.
	After time.Duration
	// Cause is the normalized error that carried the hint.
	Cause error
}

// Error implements the error interface.
func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("retry after %s: %v", e.After, e.Cause)
}

// Unwrap exposes the wrapped cause.
func (e *RetryAfterError) Unwrap() error { return e.Cause }

// RetryAfter extracts a provider-requested delay from err, if present.
func RetryAfter(err error) (time.Duration, bool) {
	var ra *RetryAfterError
	if errors.As(err, &ra) && ra.After > 0 {
		return ra.After, true
	}
	return 0, false
}

// codeForStatus maps an HTTP status to a normalized code.
func codeForStatus(status int, vendorCode, vendorType string) domain.ErrorCode {
	lowerCode := strings.ToLower(vendorCode)
	lowerType := strings.ToLower(vendorType)

	switch status {
	case http.StatusBadRequest:
		if looksLikeContextOverflow(lowerCode) || looksLikeContextOverflow(lowerType) {
			return domain.ErrCodeContextLength
		}
		if strings.Contains(lowerCode, "content_policy") || strings.Contains(lowerCode, "content_filter") {
			return domain.ErrCodeContentFiltered
		}
		return domain.ErrCodeInvalidRequest
	case http.StatusUnauthorized:
		return domain.ErrCodeAuthentication
	case http.StatusForbidden:
		// A provider's own moderation block arrives as 403 with a policy code.
		if strings.Contains(lowerCode, "content") || strings.Contains(lowerType, "content") {
			return domain.ErrCodeContentFiltered
		}
		return domain.ErrCodePermission
	case http.StatusNotFound:
		return domain.ErrCodeNotFound
	case http.StatusRequestEntityTooLarge:
		return domain.ErrCodeContextLength
	case http.StatusRequestTimeout:
		return domain.ErrCodeTimeout
	case http.StatusTooManyRequests:
		return domain.ErrCodeRateLimited
	case http.StatusPaymentRequired:
		return domain.ErrCodeQuotaExceeded
	case http.StatusUnprocessableEntity:
		if looksLikeContextOverflow(lowerCode) {
			return domain.ErrCodeContextLength
		}
		return domain.ErrCodeInvalidRequest
	}

	switch {
	case status >= 500 && status < 600:
		// Providers commonly overload 503 to mean "temporarily unavailable",
		// which is exactly the case fallback exists for.
		return domain.ErrCodeUpstream
	case status >= 400:
		return domain.ErrCodeInvalidRequest
	default:
		return domain.ErrCodeInternal
	}
}

// defaultMessageFor produces a useful message when the provider sent none.
func defaultMessageFor(status int, provider string) string {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Sprintf("provider %s rejected the configured credentials", provider)
	case http.StatusForbidden:
		return fmt.Sprintf("provider %s denied access to this model", provider)
	case http.StatusNotFound:
		return fmt.Sprintf("provider %s reported the model does not exist", provider)
	case http.StatusTooManyRequests:
		return fmt.Sprintf("provider %s rate limited the request", provider)
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return fmt.Sprintf("provider %s timed out", provider)
	case http.StatusPaymentRequired:
		return fmt.Sprintf("provider %s reported insufficient quota", provider)
	default:
		if status >= 500 {
			return fmt.Sprintf("provider %s returned server error %d", provider, status)
		}
		return fmt.Sprintf("provider %s returned status %d", provider, status)
	}
}

// errorBodyEnvelope covers the error shapes Synapass encounters in practice:
// OpenAI's nested object, Anthropic's nested object with a different shape, and
// a bare message string.
type errorBodyEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    any    `json:"code"`
	} `json:"error"`
	// Anthropic also uses a top-level "type" alongside "error".
	Type    string `json:"type"`
	Message string `json:"message"`
	Detail  any    `json:"detail"`
}

// parseErrorBody extracts a human message plus the vendor's code and type.
func parseErrorBody(body []byte) (message, code, errType string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", "", ""
	}

	var env errorBodyEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err == nil {
		message = strings.TrimSpace(env.Error.Message)
		if message == "" {
			message = strings.TrimSpace(env.Message)
		}
		errType = strings.TrimSpace(env.Error.Type)
		if errType == "" {
			errType = strings.TrimSpace(env.Type)
		}
		switch v := env.Error.Code.(type) {
		case string:
			code = v
		case float64:
			code = strconv.FormatFloat(v, 'f', -1, 64)
		case nil:
			code = ""
		default:
			code = fmt.Sprint(v)
		}
		if message != "" {
			return message, code, errType
		}
	}

	// Not a JSON envelope: fall back to the raw body, truncated so a provider
	// that returns an HTML error page does not flood the logs.
	return truncateMessage(trimmed, 512), code, errType
}

// looksLikeContextOverflow detects the several ways providers phrase "the prompt
// is too long". This is worth special-casing because context overflow is
// fallback-eligible (a bigger model can serve it) whereas a generic 400 is not.
func looksLikeContextOverflow(s string) bool {
	s = strings.ToLower(s)
	needles := []string{
		"context_length", "context length", "maximum context", "max context",
		"too many tokens", "token limit", "context window", "reduce the length",
		"prompt is too long", "input is too long", "exceeds the maximum number of tokens",
	}
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func truncateMessage(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// parseRetryAfter parses the Retry-After header, which may be a delay in seconds
// or an HTTP date.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(value, 64); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		d := time.Until(when)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// NormalizeTransportError converts a net/http transport failure into a
// normalized error.
//
// Context cancellation is separated from deadline expiry because they mean
// different things: a cancelled context is a client that went away (not
// retryable, not fallback-eligible) whereas a deadline is the gateway's own
// budget expiring (also not retryable, because retrying spends more of a budget
// that is already gone).
func NormalizeTransportError(provider, model string, attempt int, err error) *domain.Error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, context.Canceled):
		return domain.NewError(domain.ErrCodeCanceled, "the client canceled the request").
			WithProvider(provider, model, attempt).Wrap(err)
	case errors.Is(err, context.DeadlineExceeded):
		return domain.NewError(domain.ErrCodeTimeout,
			fmt.Sprintf("request to provider %s exceeded the time budget", provider)).
			WithStatus(http.StatusGatewayTimeout).
			WithProvider(provider, model, attempt).Wrap(err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return domain.NewError(domain.ErrCodeTimeout,
			fmt.Sprintf("provider %s timed out", provider)).
			WithStatus(http.StatusGatewayTimeout).
			WithProvider(provider, model, attempt).Wrap(err)
	}

	// Certificate and DNS problems look like generic errors but are almost
	// always permanent misconfiguration, so they are labelled distinctly.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"):
		return domain.NewError(domain.ErrCodeAuthentication,
			fmt.Sprintf("TLS verification failed for provider %s", provider)).
			WithProvider(provider, model, attempt).Wrap(err)
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "server misbehaving"):
		return domain.NewError(domain.ErrCodeUnavailable,
			fmt.Sprintf("provider %s host could not be resolved", provider)).
			WithProvider(provider, model, attempt).Wrap(err)
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "connection reset"):
		return domain.NewError(domain.ErrCodeUnavailable,
			fmt.Sprintf("provider %s refused the connection", provider)).
			WithProvider(provider, model, attempt).Wrap(err)
	}

	return domain.NewError(domain.ErrCodeUnavailable,
		fmt.Sprintf("transport error calling provider %s: %s", provider, msg)).
		WithProvider(provider, model, attempt).Wrap(err)
}

// NormalizeDecodeError reports a response body that could not be decoded. This
// is treated as an upstream fault rather than an internal one because the
// gateway's own decoding is covered by tests; a malformed body means the
// upstream is not speaking the protocol it advertises.
func NormalizeDecodeError(provider, model string, attempt int, err error, body []byte) *domain.Error {
	preview := sanitizeProviderMessage(truncateMessage(strings.TrimSpace(string(body)), 256))
	msg := fmt.Sprintf("provider %s returned a response that could not be decoded", provider)
	if preview != "" {
		msg = fmt.Sprintf("%s: %s", msg, preview)
	}
	return domain.NewError(domain.ErrCodeUpstream, msg).
		WithStatus(http.StatusBadGateway).
		WithProvider(provider, model, attempt).Wrap(err)
}

// credentialPatterns match the shapes a credential takes when an upstream echoes
// its own request back in an error message. Gateways commonly see bodies such as
// "Incorrect API key provided: sk-live-..." or an echoed Authorization header;
// relaying that to a tenant would leak one customer's secret to another (or to
// whoever reads the client's logs).
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)\b(?:sk|pk|rk)-[A-Za-z0-9_-]{12,}`),
	// The optional quotes cover a JSON body, where the field name and value
	// are both quoted: {"api_key":"sk-..."}.
	regexp.MustCompile(`(?i)\b(?:x-api-key|api[-_]?key|authorization|access[-_]?token)\b["']?\s*[:=]\s*["']?[^\s"',}]+`),
}

// sanitizeProviderMessage redacts credential-shaped substrings from an upstream
// message before it is stored on an error or returned to a client.
//
// Over-redaction is acceptable here: the message is a diagnostic, and a
// slightly less specific message is a much smaller cost than leaking a key.
func sanitizeProviderMessage(message string) string {
	if message == "" {
		return ""
	}
	for _, pattern := range credentialPatterns {
		message = pattern.ReplaceAllString(message, "[redacted]")
	}
	return message
}

// ReadBody reads and closes a response body with a hard cap, so a misbehaving
// upstream cannot exhaust gateway memory.
func ReadBody(resp *http.Response, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 1 << 20
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// IsRetryableStatus reports whether an HTTP status is worth another attempt
// against the same provider. It exists so the health prober, which does not go
// through the full normalization path, can classify a probe result consistently.
func IsRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusRequestTimeout ||
		status == http.StatusInternalServerError ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}
