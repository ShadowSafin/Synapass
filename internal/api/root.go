package api

import (
	"net/http"
	"strings"
	"time"
)

// handleRoot serves GET /.
//
// Synapass is an API gateway, so it has no user-facing page at the root: the
// useful surfaces are /v1/chat/completions, /v1/models and the admin API. But a
// gateway that is also reachable from a browser — which is exactly what a
// temporary tunnel invites — must not answer a bare URL with a bare 404. That
// reads as a broken deployment when the deployment is in fact healthy.
//
// So the root answers honestly: it identifies the service, reports that it is
// alive, and lists where to go next. It exposes nothing that /health does not
// already expose, and it never requires a credential — it is the same
// information a load balancer can already fetch.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// A JSON client asking for the root wants machine-readable output; a
	// browser wants something legible. Content negotiation on Accept is the
	// only signal that distinguishes them reliably.
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "synapass",
			"status":  "ok",
			"version": s.version,
			"uptime":  s.uptime().Round(time.Second).String(),
			"endpoints": map[string]string{
				"health":    "/health",
				"ready":     "/ready",
				"version":   "/version",
				"models":    "/v1/models (requires an API key)",
				"chat":      "/v1/chat/completions (OpenAI-compatible, requires an API key)",
				"admin":     "/admin/v1 (requires the admin credential)",
				"dashboard": "the control-plane dashboard, served separately",
			},
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(rootPage(s.version.Version)))
}

// wantsJSON reports whether the client asked for JSON rather than HTML.
func wantsJSON(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if accept == "" {
		// No Accept header at all: curl-style clients want JSON.
		return true
	}
	if strings.Contains(accept, "text/html") {
		return false
	}
	return true
}

// rootPage renders the browser-facing landing page. It is a constant template
// with one substituted version string rather than a templating dependency: the
// page is a status card, not an application.
func rootPage(version string) string {
	if version == "" {
		version = "dev"
	}
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>Synapass</title>
<style>
  :root { color-scheme: dark; }
  body {
    margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
    background: #0a0a0b; color: #e7e7ea;
    font: 15px/1.55 ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
  }
  main { width: min(560px, calc(100% - 2.5rem)); padding: 2rem 0; }
  h1 { margin: 0 0 .25rem; font-size: 1.35rem; letter-spacing: -0.01em; }
  .sub { margin: 0 0 1.75rem; color: #9a9aa4; font-size: .875rem; }
  .pill {
    display: inline-flex; align-items: center; gap: .4rem; margin-bottom: 1.5rem;
    padding: .2rem .6rem; border-radius: 999px; font-size: .75rem;
    background: rgba(34,197,94,.12); color: #4ade80; border: 1px solid rgba(34,197,94,.25);
  }
  ul { list-style: none; margin: 0; padding: 0; border-top: 1px solid #232329; }
  li { border-bottom: 1px solid #232329; }
  .row { display: flex; gap: 1rem; align-items: baseline; padding: .7rem 0; }
  code { font: 13px/1.4 ui-monospace, SFMono-Regular, Menlo, monospace; color: #d4d4d8; }
  .note { margin-left: auto; color: #6f6f7a; font-size: .8125rem; text-align: right; }
  footer { margin-top: 1.75rem; color: #6f6f7a; font-size: .8125rem; }
</style>
</head>
<body>
<main>
  <div class="pill">● online</div>
  <h1>Synapass ` + htmlEscape(version) + `</h1>
  <p class="sub">This is an API gateway. Point your client at one of the endpoints below.</p>
  <ul>
    <li><div class="row"><code>POST /v1/chat/completions</code><span class="note">OpenAI-compatible<br>API key</span></div></li>
    <li><div class="row"><code>GET /v1/models</code><span class="note">API key</span></div></li>
    <li><div class="row"><code>GET /health</code><span class="note">liveness, public</span></div></li>
    <li><div class="row"><code>GET /ready</code><span class="note">readiness, public</span></div></li>
    <li><div class="row"><code>GET /admin/v1</code><span class="note">admin<br>credential</span></div></li>
  </ul>
  <footer>The control-plane dashboard is served separately. Authentication is enforced on every endpoint except the health probes.</footer>
</main>
</body>
</html>`
}

// htmlEscape escapes the few characters that could break out of the page text.
func htmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(s)
}
