package domain

import "time"

// ---------------------------------------------------------------------------
// Temporary public tunnels (Cloudflare quick tunnels via cloudflared).
//
// A tunnel session is disposable by design: cloudflared mints a random
// *.trycloudflare.com URL, proxies it to one local Synapass service, and
// the URL dies with the process. The database row is the audit trail, not a
// permanent binding — nothing here manages custom domains.
// ---------------------------------------------------------------------------

// Tunnel statuses. Only one session per gateway may be running at a time;
// the database may hold any number of terminal rows for history.
type TunnelStatus string

const (
	// TunnelStarting means the cloudflared process was spawned but has not
	// printed a public URL yet.
	TunnelStarting TunnelStatus = "starting"
	// TunnelRunning means a public URL was captured and the process is alive.
	TunnelRunning TunnelStatus = "running"
	// TunnelStopped is a clean, operator-requested (or shutdown) teardown.
	TunnelStopped TunnelStatus = "stopped"
	// TunnelFailed means the process exited unexpectedly or never produced a
	// URL. The row records why so the dashboard can say more than "broken".
	TunnelFailed TunnelStatus = "failed"
)

// Valid reports whether the status is known.
func (s TunnelStatus) Valid() bool {
	switch s {
	case TunnelStarting, TunnelRunning, TunnelStopped, TunnelFailed:
		return true
	default:
		return false
	}
}

// Terminal reports whether the session will never change state again.
func (s TunnelStatus) Terminal() bool {
	return s == TunnelStopped || s == TunnelFailed
}

// Named tunnel targets. A custom "host:port" is also accepted when it points
// at loopback; anything else is rejected before a process is ever spawned.
const (
	TunnelTargetGateway   = "gateway"
	TunnelTargetDashboard = "dashboard"
)

// TunnelSession is one temporary exposure: what is public, what it points
// at, and how it ended. The public URL is the only Cloudflare-assigned value;
// everything else is Synapass's own bookkeeping.
type TunnelSession struct {
	ID string `json:"id"`
	// TenantID attributes the session when an operator key is tenant-scoped.
	// Empty for the bootstrap admin key.
	TenantID string `json:"tenant_id,omitempty"`
	// Target is the requested target: "gateway", "dashboard" or "host:port".
	Target string `json:"target"`
	// TargetAddr is the resolved local address cloudflared proxies to.
	TargetAddr string `json:"target_addr"`
	// PublicURL is the minted https://*.trycloudflare.com URL. Empty until
	// the process prints it.
	PublicURL string       `json:"public_url,omitempty"`
	Status    TunnelStatus `json:"status"`
	// StartedAt is when the process was spawned; URLAt is when the public URL
	// was first captured. The gap between them is the tunnel setup latency.
	StartedAt *time.Time `json:"started_at,omitempty"`
	URLAt     *time.Time `json:"url_at,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	// StopReason is operator-requested, shutdown, superseded, failed or expired.
	StopReason string `json:"stop_reason,omitempty"`
	// LastError is the most recent process or supervision error, if any.
	LastError string `json:"last_error,omitempty"`
	// Reconnects counts supervisor-initiated restarts of the same session.
	Reconnects int `json:"reconnects"`
	// CreatedBy labels the actor (admin key label).
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Active reports whether the session currently exposes anything.
func (s *TunnelSession) Active() bool {
	return s != nil && (s.Status == TunnelStarting || s.Status == TunnelRunning)
}

// Duration reports how long the session has been (or was) alive.
func (s *TunnelSession) Duration(now time.Time) time.Duration {
	if s == nil || s.StartedAt == nil {
		return 0
	}
	end := now
	if s.StoppedAt != nil {
		end = *s.StoppedAt
	}
	if end.Before(*s.StartedAt) {
		return 0
	}
	return end.Sub(*s.StartedAt)
}

// TunnelEvent is the NATS payload for a tunnel lifecycle transition. It
// carries the same summary the admin API returns, so the event stream and
// the API cannot disagree about what happened.
type TunnelEvent struct {
	// Event is created, url, restarted, stopped or failed.
	Event     string         `json:"event"`
	Session   *TunnelSession `json:"session,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}
