package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/dashboardauth"
	"github.com/shadowsafin/synapass/internal/domain"
)

// Dashboard operator authentication.
//
// These routes sit *outside* the admin group and outside adminMiddleware on
// purpose: they are how an operator obtains a session in the first place, so
// requiring an existing credential to reach them would be circular.
//
// What protects them is that they do not need one. Setup is latched shut after
// the first operator exists, login is rate limited and locked out, and the only
// thing they disclose is whether setup is still pending.
//
// The admin key continues to work for the admin API itself. This is a gate on
// the *dashboard*, not a replacement for programmatic access, which is why the
// smoke suite and CI are unaffected.

const (
	// sessionCookieDefault is the fallback cookie name when configuration omits it.
	sessionCookieDefault = "synapass_session"
)

// authStateResponse is the body of GET /admin/v1/auth/state.
//
// Deliberately minimal. The console needs to know whether to render setup, login
// or the application, and nothing more; returning the operator list or any
// account detail here would leak it to an unauthenticated caller.
type authStateResponse struct {
	SetupRequired bool                      `json:"setup_required"`
	Authenticated bool                      `json:"authenticated"`
	Username      string                    `json:"username,omitempty"`
	Policy        dashboardauth.Description `json:"policy"`
}

// setupRequest is the body of POST /admin/v1/auth/setup.
type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Confirm  string `json:"confirm"`
}

// loginRequest is the body of POST /admin/v1/auth/login.
//
// Confirm is declared and then ignored. The setup and login screens are the same
// component, so it posts the same three fields to both endpoints, and the decoder
// rejects unknown fields by design. Accepting the field here is clearer than
// making the client remember to strip it.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Confirm  string `json:"confirm,omitempty"`
}

// currentUser is the body of GET /admin/v1/auth/me.
type currentUser struct {
	Username    string     `json:"username"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// handleAuthState serves GET /admin/v1/auth/state.
func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	if !s.config.Admin.Enabled || !s.config.Admin.DashboardAuth.Enabled {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"message": "dashboard authentication is not enabled",
			"type":    "not_found",
			"code":    string(domain.ErrCodeNotFound),
		}})
		return
	}

	state, err := s.dashboardAuth.State(r.Context())
	if err != nil {
		s.logger.Warn("could not read dashboard auth state", "error", err)
		writeError(w, domain.NewError(domain.ErrCodeInternal,
			"could not determine whether setup is required"), nil)
		return
	}

	response := authStateResponse{
		SetupRequired: state.SetupRequired,
		Policy:        s.dashboardAuth.Describe(),
	}

	// Validating the cookie opportunistically means a refresh of an already-signed
	// in console does not bounce to the login screen.
	if user, err := s.dashboardAuthFromCookie(r); err == nil && user != nil {
		response.Authenticated = true
		response.Username = user.Username
	}

	writeJSON(w, http.StatusOK, response)
}

// handleAuthSetup serves POST /admin/v1/auth/setup: the first-run flow.
//
// It creates exactly one operator, closes setup permanently, and signs the new
// operator in. A second call returns 409, which is the correct answer rather than
// an error: there is already an owner of this console.
func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if !s.dashboardAuthEnabled(w) {
		return
	}

	var body setupRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "the request body is not valid JSON"), nil)
		return
	}

	result, err := s.dashboardAuth.Setup(r.Context(), body.Username, body.Password, body.Confirm,
		s.clientInfo(r))
	if err != nil {
		s.writeAuthError(w, r, err, "setup")
		return
	}

	s.setSessionCookie(w, r, result)
	s.recordAuthOutcome("setup_success", "")

	writeJSON(w, http.StatusCreated, map[string]any{
		"username":   result.User.Username,
		"expires_at": result.ExpiresAt,
	})
}

// handleAuthLogin serves POST /admin/v1/auth/login.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.dashboardAuthEnabled(w) {
		return
	}

	var body loginRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "the request body is not valid JSON"), nil)
		return
	}

	result, err := s.dashboardAuth.Login(r.Context(), body.Username, body.Password, s.clientInfo(r))
	if err != nil {
		s.writeAuthError(w, r, err, "login")
		return
	}

	s.setSessionCookie(w, r, result)
	s.recordAuthOutcome("login_success", result.User.Username)

	writeJSON(w, http.StatusOK, map[string]any{
		"username":   result.User.Username,
		"expires_at": result.ExpiresAt,
	})
}

// handleAuthLogout serves POST /admin/v1/auth/logout.
//
// Logout always returns 204, even for a token that was never valid. Reporting
// "that session did not exist" would confirm which tokens are real.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !s.dashboardAuthEnabled(w) {
		return
	}

	cookie := s.sessionCookieName()
	if token := readCookie(r, cookie); token != "" {
		if err := s.dashboardAuth.Logout(r.Context(), token); err != nil {
			s.logger.Warn("could not revoke dashboard session", "error", err)
		}
	}
	s.clearSessionCookie(w, r)
	s.recordAuthOutcome("logout", "")

	w.WriteHeader(http.StatusNoContent)
}

// handleAuthMe serves GET /admin/v1/auth/me.
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if !s.dashboardAuthEnabled(w) {
		return
	}

	user, err := s.dashboardAuthFromCookie(r)
	if err != nil || user == nil {
		writeError(w, dashboardauth.ErrInvalidToken, nil)
		return
	}

	writeJSON(w, http.StatusOK, currentUser{
		Username:    user.Username,
		CreatedAt:   user.CreatedAt,
		LastLoginAt: user.LastLoginAt,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// dashboardAuthEnabled guards the routes and reports a consistent 404 when the
// feature is off, so its absence is not distinguishable from a wrong path.
func (s *Server) dashboardAuthEnabled(w http.ResponseWriter) bool {
	if s.config.Admin.Enabled && s.config.Admin.DashboardAuth.Enabled && s.dashboardAuth != nil {
		return true
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
		"message": "dashboard authentication is not enabled",
		"type":    "not_found",
		"code":    string(domain.ErrCodeNotFound),
	}})
	return false
}

// dashboardAuthFromCookie resolves the session cookie to a user.
//
// The cookie is absent or invalid in both the normal anonymous case and the
// tampered case; neither is distinguished to the caller.
func (s *Server) dashboardAuthFromCookie(r *http.Request) (*domain.DashboardUser, error) {
	if s.dashboardAuth == nil {
		return nil, dashboardauth.ErrInvalidToken
	}
	token := readCookie(r, s.sessionCookieName())
	if token == "" {
		return nil, dashboardauth.ErrInvalidToken
	}
	return s.dashboardAuth.Authenticate(r.Context(), token)
}

func (s *Server) sessionCookieName() string {
	name := strings.TrimSpace(s.config.Admin.DashboardAuth.CookieName)
	if name == "" {
		return sessionCookieDefault
	}
	return name
}

// setSessionCookie writes the session cookie.
//
// Secure is decided from the request rather than from the environment name. A
// deployment labelled production but served over plain HTTP — which is what
// `docker compose up` gives you — would otherwise set a Secure cookie that the
// browser silently discards, leaving the console unusable with no error to
// explain why. Detecting the actual scheme also gets it right behind a
// TLS-terminating proxy, where the gateway only sees X-Forwarded-Proto.
//
// Configuration can still force it on for a deployment that is HTTPS on every
// path including ones the gateway cannot see.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, result *dashboardauth.Result) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.sessionCookieName(),
		Value:    result.Token,
		Path:     "/",
		Expires:  result.ExpiresAt,
		MaxAge:   int(time.Until(result.ExpiresAt).Seconds()),
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// cookieSecure reports whether the session cookie should carry the Secure
// attribute.
func (s *Server) cookieSecure(r *http.Request) bool {
	if s.config.Admin.DashboardAuth.CookieSecure {
		return true
	}
	if r.TLS != nil {
		return true
	}
	// Honour the forwarded scheme only from a trusted proxy, for the same reason
	// X-Forwarded-For is: an untrusted header is a client-supplied claim.
	if len(s.trustedProxies) > 0 {
		if ip := remoteIP(r, s.trustedProxies); ip != nil {
			for _, network := range s.trustedProxies {
				if network.Contains(ip) {
					return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
				}
			}
		}
	}
	return false
}

// clearSessionCookie removes the cookie with matching attributes.
//
// Path, SameSite and Secure must match the cookie it replaces, or the browser
// keeps the original and the operator appears unable to log out.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.sessionCookieName(),
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// clientInfo captures the attribution stored on a session.
func (s *Server) clientInfo(r *http.Request) dashboardauth.ClientInfo {
	rc := requestContext(r.Context())
	ip := ""
	if rc != nil {
		ip = rc.ClientIP
	}
	ua := ""
	if rc != nil {
		ua = rc.UserAgent
	}
	return dashboardauth.ClientInfo{IP: ip, UserAgent: ua}
}

// writeAuthError maps a flow failure onto HTTP.
//
// The two credential failures are deliberately indistinguishable in the response
// body: "the username or password is incorrect" for both, so the form cannot be
// used to discover which accounts exist. The rate-limited case is different and
// deliberately explicit, because silently returning "incorrect" while an account
// is locked out would leave a legitimate operator with no way to tell a lockout
// from a typo.
func (s *Server) writeAuthError(w http.ResponseWriter, r *http.Request, err error, action string) {
	normalized := domain.AsError(err)

	if normalized.Code == domain.ErrCodeRateLimited {
		if seconds := int(s.config.Admin.DashboardAuth.LockoutDuration.Std()); seconds > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
	}
	if normalized.Code == domain.ErrCodePermission && strings.Contains(normalized.Message, "setup") {
		s.recordAuthOutcome("setup_rejected", "")
		writeError(w, normalized, nil)
		return
	}

	if normalized.Code == domain.ErrCodeAuthentication || normalized.Code == domain.ErrCodePermission {
		s.recordAuthOutcome(action+"_failed", "")
		writeError(w, normalized, nil)
		return
	}

	s.recordAuthOutcome(action+"_error", "")
	writeError(w, normalized, nil)
}

// decodeJSON is the package's strict request decoder, bounded so a login attempt
// cannot be used to push an unbounded body at the gateway.
func decodeJSON(r *http.Request, v any) error {
	return decodeJSONBody(r, 1<<16, v)
}

// recordAuthOutcome publishes a counter for the auth flow.
//
// Failures are counted by outcome, never by username: a label per attempted
// username would let an attacker inflate metric cardinality by guessing names.
func (s *Server) recordAuthOutcome(outcome, username string) {
	if s.metrics != nil {
		s.metrics.ObserveDashboardAuth(outcome)
	}
	if username != "" {
		s.logger.Info("dashboard authentication", "outcome", outcome, "username", username)
		return
	}
	s.logger.Info("dashboard authentication", "outcome", outcome)
}

// readCookie returns a cookie value, treating a malformed cookie as absent.
func readCookie(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// remoteIP extracts the peer address, ignoring any forwarded header.
func remoteIP(r *http.Request, _ []*net.IPNet) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}
