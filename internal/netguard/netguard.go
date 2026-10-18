// Package netguard validates outbound URLs owned by operators or administrators
// before the gateway is asked to connect to them.
//
// Two callers need this and they need slightly different policies:
//
//   - benchmark notification targets are pure user input, so they must be
//     public destinations: loopback, private, link-local and cloud metadata
//     addresses are all refused, and the refusal is re-checked at dial time so
//     a DNS answer that changes after validation cannot bypass it.
//   - provider base URLs are operator configuration and a local model runtime
//     (Ollama, vLLM on the LAN) is a first-class deployment. Loopback and
//     private ranges therefore stay allowed, but link-local/metadata addresses
//     never do: no inference provider legitimately lives at 169.254.169.254.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxRedirects bounds a webhook delivery's redirect chain. More than a couple of
// hops is a sign of an open redirect being used as a proxy, not of a moved
// endpoint.
const maxRedirects = 3

// ValidateHTTPURL applies the structural rules shared by every outbound URL: an
// absolute http(s) URL with a host and no embedded credentials.
//
// Userinfo is refused because "https://user:pass@internal/" is how a URL hides
// its real destination from a reader, and because credentials belong in the
// provider credential store, not in a URL that is logged.
func ValidateHTTPURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("URL is not valid: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL must use http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("URL is missing a host")
	}
	if u.User != nil {
		return nil, fmt.Errorf("URL must not contain credentials")
	}
	return u, nil
}

// IsBlockedIP reports whether an address must never be reached by a webhook.
//
// The link-local range is the important one: every major cloud exposes instance
// credentials at 169.254.169.254 (and fd00:ec2::254 on AWS IPv6), which is the
// usual objective of an SSRF primitive. The private and loopback ranges are
// included because a webhook has no legitimate reason to reach them.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// 100.64.0.0/10 (carrier-grade NAT) is not covered by IsPrivate and is
	// routinely the address of internal services on container networks.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		// 0.0.0.0/8: only 0.0.0.0 itself is "unspecified"; the rest of the
		// range is not routable and should not be a destination either.
		if v4[0] == 0 {
			return true
		}
		// 198.18.0.0/15 (benchmarking) and 192.0.0.0/24 (IETF protocol
		// assignments) are not public destinations.
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return true
		}
		if v4[0] == 192 && v4[1] == 0 && v4[2] == 0 {
			return true
		}
	}
	return false
}

// IsBlockedProviderIP is the provider policy: link-local, unspecified,
// multicast and CGNAT are refused, while loopback and private addresses remain
// usable for self-hosted runtimes.
func IsBlockedProviderIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		if v4[0] == 0 {
			return true
		}
	}
	return false
}

// ValidatePublicURL validates a webhook target and resolves it.
//
// Resolution happens here so a hostname that points at an internal address
// (for example an attacker-controlled name resolving to 10.0.0.5) is refused at
// write time. The dial-time guard in NewPublicClient repeats the check, because
// a name can be re-pointed after this call returns.
func ValidatePublicURL(ctx context.Context, raw string) (*url.URL, error) {
	u, err := ValidateHTTPURL(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return nil, fmt.Errorf("URL points at a non-public address")
		}
		return u, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, fmt.Errorf("URL host %q cannot be resolved", host)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("URL host %q has no addresses", host)
	}
	for _, addr := range addrs {
		if IsBlockedIP(addr.IP) {
			return nil, fmt.Errorf("URL host %q resolves to a non-public address", host)
		}
	}
	return u, nil
}

// ValidateProviderURL validates a provider base URL without requiring the host
// to be publicly routable: a local runtime is a supported deployment.
//
// IP literals are checked directly. Hostnames are checked only if they resolve,
// so creating a provider for a DNS name that is not yet available still works;
// the address is re-checked when the adapter connects.
func ValidateProviderURL(ctx context.Context, raw string) (*url.URL, error) {
	u, err := ValidateHTTPURL(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedProviderIP(ip) {
			return nil, fmt.Errorf("provider base_url points at a link-local or non-routable address")
		}
		return u, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host); err == nil {
		for _, addr := range addrs {
			if IsBlockedProviderIP(addr.IP) {
				return nil, fmt.Errorf("provider base_url resolves to a link-local or non-routable address")
			}
		}
	}
	return u, nil
}

// NewPublicClient returns an HTTP client whose dialer refuses blocked addresses
// at connect time.
//
// Validating the name once is not enough: DNS can be re-pointed between the
// admin write and the delivery, and a redirect can send the request somewhere
// else entirely. Resolving inside the dialer and then dialing the vetted IP
// removes both windows.
func NewPublicClient(timeout time.Duration) *http.Client {
	dialer := &guardedDialer{
		dial: DialContext(&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, IsBlockedIP),
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to a non-http(s) URL refused")
			}
			if req.URL.User != nil {
				return fmt.Errorf("redirect carrying credentials refused")
			}
			return nil
		},
	}
}

// DialContext returns a dial function that resolves the host itself and connects
// only to addresses accepted by block.
//
// Dialing the resolved IP rather than the name is what closes the DNS-rebinding
// window: the address that was checked is the address that is connected to.
func DialContext(base *net.Dialer, block func(net.IP) bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []net.IP
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("host %q has no addresses", host)
		}
		var lastErr error
		for _, ip := range ips {
			if block(ip) {
				return nil, fmt.Errorf("refusing to connect to blocked address %s", ip)
			}
			conn, err := base.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// guardedDialer adapts DialContext to the field shape http.Transport expects.
type guardedDialer struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (d *guardedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.dial(ctx, network, addr)
}
