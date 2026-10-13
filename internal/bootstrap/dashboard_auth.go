package bootstrap

import (
	"context"
	"log/slog"
	"time"

	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/dashboardauth"
	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/storage"
)

// Construction of the console authentication service.
//
// It lives in bootstrap rather than in the package itself because it is the only
// place that holds all three stores: the dashboard user store, the session store
// and the settings table that carries the first-run latch.

// DashboardAuditSink writes console authentication events into the existing audit
// trail.
//
// Login, logout, setup and password changes are audited for the same reason
// control-plane writes are: "who signed in, and when" is exactly the question an
// operator needs answered after an incident, and it cannot be reconstructed from
// the request log because the request log does not carry the identity.
//
// No password or token is ever passed to this sink. The service passes only event
// names, usernames and counters.
type DashboardAuditSink struct {
	audit    *storage.AuditRepository
	tenantID string
	logger   *slog.Logger
}

// NewDashboardAuditSink constructs the sink. A nil repository disables auditing
// rather than failing startup, matching the gateway's existing rule that an audit
// write failure must never take down the action it accompanies.
func NewDashboardAuditSink(audit *storage.AuditRepository, logger *slog.Logger) *DashboardAuditSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &DashboardAuditSink{audit: audit, logger: logger}
}

// Record writes one audit event.
func (s *DashboardAuditSink) Record(
	ctx context.Context, event, actor, resource, resourceID string, details map[string]any,
) {
	if s == nil || s.audit == nil {
		return
	}

	action, auditResource := classifyDashboardEvent(event, resource)

	rec := &domain.AuditEvent{
		ID:         domain.NewID(),
		Action:     action,
		Resource:   auditResource,
		ResourceID: resourceID,
		TenantID:   s.tenantID,
		// ActorLabel rather than ActorKeyID: a console operator has no API key,
		// and the label is captured at write time so the event stays readable.
		ActorLabel: actor,
		CreatedAt:  time.Now().UTC(),
	}
	if details != nil {
		rec.After = details
	}

	if err := s.audit.Insert(ctx, rec); err != nil {
		// Logged, not returned. Failing the login because the audit write failed
		// would mean a database hiccup locks every operator out of the console.
		s.logger.Warn("could not write a dashboard authentication audit event",
			"event", event, "username", actor, "error", err)
	}
}

// classifyDashboardEvent maps a flow event onto the audit vocabulary.
func classifyDashboardEvent(event, resource string) (domain.AuditAction, domain.AuditResource) {
	auditResource := domain.ResourceDashboardUser
	if resource == string(domain.ResourceDashboardSession) {
		auditResource = domain.ResourceDashboardSession
	}

	switch event {
	case "dashboard_setup":
		return domain.AuditCreate, auditResource
	case "dashboard_login":
		return domain.AuditLogin, auditResource
	case "dashboard_login_failed":
		return domain.AuditDeny, auditResource
	case "dashboard_logout":
		return domain.AuditLogout, auditResource
	case "dashboard_password_changed":
		return domain.AuditPasswordChange, auditResource
	case "dashboard_user_enabled":
		return domain.AuditUpdate, auditResource
	default:
		return domain.AuditUpdate, auditResource
	}
}

// BuildDashboardAuth constructs the console authentication service.
//
// It returns nil when the feature is off or when the required stores are absent,
// which leaves the login surface unmounted rather than half-working. A console
// that cannot authenticate anyone is worse than one that refuses to offer login.
func BuildDashboardAuth(
	cfg *config.Config,
	repos *storage.Repositories,
	logger *slog.Logger,
) *dashboardauth.Service {
	if cfg == nil || repos == nil || !cfg.Admin.Enabled || !cfg.Admin.DashboardAuth.Enabled {
		return nil
	}

	dashboardCfg := cfg.Admin.DashboardAuth
	if dashboardCfg.CookieName == "" {
		dashboardCfg.CookieName = "synapass_session"
	}

	// The password floor is a floor, not a preference: accepting a lower value
	// would mean storing digests that are cheap to crack, so it is clamped rather
	// than honoured.
	rules := dashboardauth.PasswordRules{
		MinLength: dashboardCfg.MinPasswordLength,
		MaxLength: dashboardCfg.MaxPasswordLength,
	}
	if rules.MinLength < dashboardauth.DefaultPasswordRules().MinLength {
		rules.MinLength = dashboardauth.DefaultPasswordRules().MinLength
	}
	if rules.MaxLength <= 0 {
		rules.MaxLength = dashboardauth.DefaultPasswordRules().MaxLength
	}

	service := dashboardauth.NewService(dashboardauth.Options{
		Users:    repos.DashboardUsers,
		Sessions: repos.DashboardSessions,
		Latch:    repos.Settings,
		Audit:    NewDashboardAuditSink(repos.Audit, logger),
		Logger:   logger,

		PasswordRules:     rules,
		SessionTTL:        dashboardCfg.SessionTTL.Std(),
		IdleTTL:           dashboardCfg.IdleTTL.Std(),
		MaxFailedAttempts: dashboardCfg.MaxFailedAttempts,
		LockoutDuration:   dashboardCfg.LockoutDuration.Std(),
		MaxLockout:        dashboardCfg.MaxLockoutDuration.Std(),
	})

	return service
}
