package dashboardauth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// The operator-facing auth flow: first-run setup, login, and session validation.
//
// # Setup is latched, not merely "no users yet"
//
// Gating setup on the absence of rows would mean that deleting every operator
// silently reopens first-run setup to anyone who can reach the console. The latch
// in the settings table is what closes it permanently; reopening requires an
// explicit administrative act.

// SetupLatchKey is the settings key holding the first-run completion timestamp.
const SetupLatchKey = "dashboard_auth.setup_completed_at"

// UserStore persists operator accounts.
type UserStore interface {
	Create(ctx context.Context, u *domain.DashboardUser) (*domain.DashboardUser, error)
	Count(ctx context.Context) (int, error)
	ByUsername(ctx context.Context, username string) (*domain.DashboardUser, error)
	ByID(ctx context.Context, id string) (*domain.DashboardUser, error)
	RecordLoginSuccess(ctx context.Context, id string, at time.Time) error
	RecordLoginFailure(ctx context.Context, id string, at time.Time, threshold int, lockUntil *time.Time) error
	SetPassword(ctx context.Context, id, hash string, at time.Time) error
	SetEnabled(ctx context.Context, id string, enabled bool, at time.Time) error
}

// SessionStore persists browser sessions.
type SessionStore interface {
	Create(ctx context.Context, s *domain.DashboardSession) (*domain.DashboardSession, error)
	ByTokenHash(ctx context.Context, tokenHash string) (*domain.DashboardSession, error)
	Touch(ctx context.Context, id string, at time.Time) error
	Revoke(ctx context.Context, tokenHash string, at time.Time) error
	RevokeAllForUser(ctx context.Context, userID string, at time.Time) (int64, error)
}

// LatchStore holds the first-run completion marker.
type LatchStore interface {
	Get(ctx context.Context, key string) (json.RawMessage, bool, error)
	Set(ctx context.Context, key string, value any, updatedBy string) error
}

// AuditSink records security-relevant events.
//
// Login and setup are audited, matching the gateway's existing rule that control
// plane activity is accountable. Nothing here records a password or a token.
type AuditSink interface {
	Record(ctx context.Context, event string, actor string, resource string, resourceID string, details map[string]any)
}

// ClientInfo is the best-effort client attribution attached to a session.
type ClientInfo struct {
	IP        string
	UserAgent string
}

// Options configures the service.
type Options struct {
	Users    UserStore
	Sessions SessionStore
	Latch    LatchStore
	Audit    AuditSink
	Logger   *slog.Logger

	// PasswordRules is the enforced policy. Zero value means the defaults.
	PasswordRules PasswordRules
	// SessionTTL is the absolute session lifetime.
	SessionTTL time.Duration
	// IdleTTL expires a session that long without being seen. Zero disables it.
	IdleTTL time.Duration
	// MaxFailedAttempts is the consecutive-failure count that triggers a lockout.
	MaxFailedAttempts int
	// LockoutDuration is the base lockout, doubled per extra round of failures.
	LockoutDuration time.Duration
	// MaxLockout bounds the doubling, so a sustained attack cannot produce a
	// multi-year lockout that would require manual database intervention.
	MaxLockout time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// Service implements the dashboard authentication flow.
type Service struct {
	opts Options
}

// NewService constructs the service, filling defaults for anything unset.
func NewService(opts Options) *Service {
	if opts.PasswordRules == (PasswordRules{}) {
		opts.PasswordRules = DefaultPasswordRules()
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 12 * time.Hour
	}
	if opts.MaxFailedAttempts <= 0 {
		opts.MaxFailedAttempts = 5
	}
	if opts.LockoutDuration <= 0 {
		opts.LockoutDuration = 30 * time.Second
	}
	if opts.MaxLockout <= 0 {
		opts.MaxLockout = 15 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Service{opts: opts}
}

// Errors returned by the flow.
//
// Every credential failure resolves to ErrInvalidCredentials, never to a
// distinct "no such user" or "wrong password": distinguishing them would turn the
// login form into an account-enumeration oracle.
var (
	// ErrInvalidCredentials is returned for any authentication failure.
	ErrInvalidCredentials = domain.NewError(domain.ErrCodeAuthentication, "the username or password is incorrect")
	// ErrAccountLocked is returned while a lockout window is open.
	ErrAccountLocked = domain.NewError(domain.ErrCodeRateLimited, "too many failed attempts; try again shortly")
	// ErrAccountDisabled is returned for a disabled account.
	ErrAccountDisabled = domain.NewError(domain.ErrCodePermission, "this account is disabled")
	// ErrSetupClosed is returned when first-run setup is no longer available.
	ErrSetupClosed = domain.NewError(domain.ErrCodePermission, "initial setup has already been completed")
	// ErrInvalidToken is returned for an unknown, revoked or expired session.
	ErrInvalidToken = domain.NewError(domain.ErrCodeAuthentication, "the session is not valid")
)

// State describes whether first-run setup is still available.
type State struct {
	// SetupRequired is true only before the first operator account exists and the
	// latch has not been written. It carries no information about accounts beyond
	// that, so exposing it publicly is safe.
	SetupRequired bool `json:"setup_required"`
}

// Result is a completed authentication.
type Result struct {
	User      *domain.DashboardUser
	Token     string
	ExpiresAt time.Time
}

// State reports whether the console still needs its first operator.
func (s *Service) State(ctx context.Context) (State, error) {
	latched, err := s.setupLatched(ctx)
	if err != nil {
		return State{}, err
	}
	if latched {
		return State{SetupRequired: false}, nil
	}
	count, err := s.opts.Users.Count(ctx)
	if err != nil {
		return State{}, err
	}
	return State{SetupRequired: count == 0}, nil
}

// setupLatched reports whether the first-run marker exists and is non-empty.
func (s *Service) setupLatched(ctx context.Context) (bool, error) {
	raw, found, err := s.opts.Latch.Get(ctx, SetupLatchKey)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	var stamp string
	if err := json.Unmarshal(raw, &stamp); err != nil {
		// A malformed marker is treated as "latched" rather than "absent". Failing
		// open here would re-open setup, which is the one direction that must not
		// happen by accident.
		return true, nil
	}
	return strings.TrimSpace(stamp) != "", nil
}

// Setup creates the single initial operator account and signs it in.
//
// The latch is written after the user row is created. Writing it first would
// leave an installation where setup is closed and no account exists, which is
// unrecoverable without database access; this order degrades instead to "setup
// still available", which Setup can retry.
func (s *Service) Setup(ctx context.Context, username, password, confirm string, client ClientInfo) (*Result, error) {
	now := s.opts.Now()

	latched, err := s.setupLatched(ctx)
	if err != nil {
		return nil, err
	}
	if latched {
		return nil, ErrSetupClosed
	}

	if password != confirm {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "the passwords do not match")
	}
	normalized := NormalizeUsername(username)
	if err := ValidateUsername(normalized); err != nil {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, err.Error())
	}
	if err := ValidatePassword(password, s.opts.PasswordRules); err != nil {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, err.Error())
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "could not secure the password").Wrap(err)
	}

	user, err := s.opts.Users.Create(ctx, &domain.DashboardUser{
		ID:           domain.NewID(),
		Username:     normalized,
		PasswordHash: hash,
		Enabled:      true,
		CreatedAt:    now,
	})
	if err != nil {
		return nil, err
	}

	// Close first-run setup. A concurrent second setup loses on the unique
	// username or on this latch, which is the correct outcome: there is exactly
	// one first operator.
	if err := s.opts.Latch.Set(ctx, SetupLatchKey, now.Format(time.RFC3339), normalized); err != nil {
		s.opts.Logger.Error("first-run setup succeeded but the latch could not be written",
			"username", normalized, "error", err)
		return nil, domain.NewError(domain.ErrCodeInternal,
			"the account was created but initial setup could not be closed; retry setup").Wrap(err)
	}

	s.audit(ctx, "dashboard_setup", normalized, "dashboard_user", user.ID, map[string]any{
		"created_at": now.Format(time.RFC3339),
	})

	result, err := s.issue(ctx, user, client, now)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Login verifies credentials and issues a session.
func (s *Service) Login(ctx context.Context, username, password string, client ClientInfo) (*Result, error) {
	now := s.opts.Now()
	normalized := NormalizeUsername(username)

	user, err := s.opts.Users.ByUsername(ctx, normalized)
	if err != nil {
		// Spend the hashing cost anyway. Returning immediately for an unknown
		// username would make account existence measurable in wall-clock time, and
		// would also make a spray across many usernames fast.
		_, _ = VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCredentials
	}

	if !user.Enabled {
		// Still verify the password so a disabled account is not distinguishable
		// from a wrong password by timing.
		_, _ = VerifyPassword(password, user.PasswordHash)
		return nil, ErrAccountDisabled
	}

	if user.Locked(now) {
		_, _ = VerifyPassword(password, user.PasswordHash)
		return nil, ErrAccountLocked
	}

	ok, verifyErr := VerifyPassword(password, user.PasswordHash)
	if verifyErr != nil {
		// A stored hash we cannot parse is an operator problem, not a client one,
		// but it must not be reported as "wrong password" or the operator will
		// never learn the account is broken.
		s.opts.Logger.Error("stored password hash could not be verified",
			"username", normalized, "error", verifyErr)
		return nil, domain.NewError(domain.ErrCodeInternal,
			"the stored credentials for this account are unreadable").Wrap(verifyErr)
	}

	if !ok {
		s.recordFailure(ctx, user, now)
		return nil, ErrInvalidCredentials
	}

	if err := s.opts.Users.RecordLoginSuccess(ctx, user.ID, now); err != nil {
		s.opts.Logger.Warn("could not record dashboard login success", "username", normalized, "error", err)
	}
	user.FailedAttempts = 0
	user.LockedUntil = nil
	user.LastLoginAt = &now

	s.audit(ctx, "dashboard_login", normalized, "dashboard_user", user.ID, map[string]any{
		"ip": client.IP,
	})

	return s.issue(ctx, user, client, now)
}

// recordFailure increments the counter and applies the lockout.
//
// The lockout grows with each extra round of failures, bounded by MaxLockout, so
// repeated guessing gets progressively more expensive without ever requiring
// manual intervention to recover a legitimate operator.
func (s *Service) recordFailure(ctx context.Context, user *domain.DashboardUser, now time.Time) {
	attempts := user.FailedAttempts + 1

	// The lockout instant is computed here rather than in SQL, so the backoff
	// policy lives in one place and the statement stays a plain CASE over one
	// timestamp type.
	var lockUntil *time.Time
	if lock := s.lockoutFor(attempts); lock > 0 {
		until := now.Add(lock)
		lockUntil = &until
	}

	if err := s.opts.Users.RecordLoginFailure(ctx, user.ID, now, s.opts.MaxFailedAttempts, lockUntil); err != nil {
		s.opts.Logger.Warn("could not record dashboard login failure",
			"username", user.Username, "error", err, "cause", unwrapCause(err))
		return
	}
	s.audit(ctx, "dashboard_login_failed", user.Username, "dashboard_user", user.ID, map[string]any{
		"failed_attempts": attempts,
		"locked":          lockUntil != nil,
	})
}

// unwrapCause returns the underlying error for logging.
//
// The gateway's normalized error renders only its own code and message, so a
// driver-level failure would otherwise be logged as "internal_error: record
// dashboard login failure" with no indication of why. That is precisely the
// shape of bug that reaches production unnoticed, because the request still
// returns the correct response to the client.
func unwrapCause(err error) error {
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		return unwrapped
	}
	return err
}

// lockoutFor returns the lockout for a given consecutive-failure count.
func (s *Service) lockoutFor(attempts int) time.Duration {
	if attempts < s.opts.MaxFailedAttempts {
		return 0
	}
	rounds := attempts - s.opts.MaxFailedAttempts
	lock := s.opts.LockoutDuration
	for i := 0; i < rounds && lock < s.opts.MaxLockout; i++ {
		lock *= 2
	}
	if lock > s.opts.MaxLockout {
		lock = s.opts.MaxLockout
	}
	return lock
}

// issue creates a session and returns the one-time token.
//
// The plaintext token exists only in this return value and the caller's cookie.
// Only its hash is persisted.
func (s *Service) issue(ctx context.Context, user *domain.DashboardUser, client ClientInfo, now time.Time) (*Result, error) {
	token, err := GenerateSessionToken()
	if err != nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "could not create a session").Wrap(err)
	}
	expires := now.Add(s.opts.SessionTTL)

	if _, err := s.opts.Sessions.Create(ctx, &domain.DashboardSession{
		ID:        domain.NewID(),
		UserID:    user.ID,
		TokenHash: HashSessionToken(token),
		CreatedAt: now,
		ExpiresAt: expires,
		IP:        client.IP,
		UserAgent: truncate(client.UserAgent, 512),
	}); err != nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "could not persist the session").Wrap(err)
	}

	return &Result{User: user, Token: token, ExpiresAt: expires}, nil
}

// Authenticate resolves a session token to its user.
func (s *Service) Authenticate(ctx context.Context, token string) (*domain.DashboardUser, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrInvalidToken
	}
	now := s.opts.Now()

	session, err := s.opts.Sessions.ByTokenHash(ctx, HashSessionToken(token))
	if err != nil {
		return nil, ErrInvalidToken
	}
	if !session.Active(now) {
		return nil, ErrInvalidToken
	}

	user, err := s.opts.Users.ByID(ctx, session.UserID)
	if err != nil {
		return nil, ErrInvalidToken
	}
	if !user.Enabled {
		return nil, ErrAccountDisabled
	}

	// Idle expiry is evaluated here rather than in a background job, because the
	// only moment an idle session can be rejected is when it is presented.
	if s.opts.IdleTTL > 0 && now.Sub(session.LastSeenAt) > s.opts.IdleTTL {
		return nil, ErrInvalidToken
	}

	// Best effort. A failure to stamp last-seen must not fail an authorised
	// request; it only costs the session its idle budget on the next check.
	if err := s.opts.Sessions.Touch(ctx, session.ID, now); err != nil {
		s.opts.Logger.Debug("could not stamp dashboard session activity", "error", err)
	}

	return user, nil
}

// Logout revokes the presented session.
//
// Revoking by token rather than deleting the row keeps the sign-in record, so an
// audit of who was authenticated survives the session.
func (s *Service) Logout(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	if err := s.opts.Sessions.Revoke(ctx, HashSessionToken(token), s.opts.Now()); err != nil {
		return domain.NewError(domain.ErrCodeInternal, "could not end the session").Wrap(err)
	}
	s.audit(ctx, "dashboard_logout", "", "dashboard_session", "", nil)
	return nil
}

// ChangePassword replaces a user's password and revokes their other sessions.
//
// Revoking is the point: a password change that leaves existing sessions valid
// does not actually remove access from whoever was holding one.
func (s *Service) ChangePassword(ctx context.Context, userID, current, next string) error {
	now := s.opts.Now()

	user, err := s.opts.Users.ByID(ctx, userID)
	if err != nil {
		return ErrInvalidCredentials
	}
	ok, err := VerifyPassword(current, user.PasswordHash)
	if err != nil || !ok {
		return ErrInvalidCredentials
	}
	if next != "" {
		if verr := ValidatePassword(next, s.opts.PasswordRules); verr != nil {
			return domain.NewError(domain.ErrCodeInvalidRequest, verr.Error())
		}
		if next == current {
			return domain.NewError(domain.ErrCodeInvalidRequest, "the new password must differ from the current one")
		}
	}

	hash, err := HashPassword(next)
	if err != nil {
		return domain.NewError(domain.ErrCodeInternal, "could not secure the password").Wrap(err)
	}
	if err := s.opts.Users.SetPassword(ctx, userID, hash, now); err != nil {
		return domain.NewError(domain.ErrCodeInternal, "could not update the password").Wrap(err)
	}
	if _, err := s.opts.Sessions.RevokeAllForUser(ctx, userID, now); err != nil {
		s.opts.Logger.Warn("password changed but sessions could not be revoked", "user_id", userID, "error", err)
	}
	s.audit(ctx, "dashboard_password_changed", user.Username, "dashboard_user", userID, nil)
	return nil
}

// SetEnabled enables or disables an account and evicts its sessions when it is
// disabled.
func (s *Service) SetEnabled(ctx context.Context, userID string, enabled bool) error {
	now := s.opts.Now()
	if err := s.opts.Users.SetEnabled(ctx, userID, enabled, now); err != nil {
		return domain.NewError(domain.ErrCodeInternal, "could not update the account").Wrap(err)
	}
	if !enabled {
		if _, err := s.opts.Sessions.RevokeAllForUser(ctx, userID, now); err != nil {
			s.opts.Logger.Warn("account disabled but sessions could not be revoked", "user_id", userID, "error", err)
		}
	}
	s.audit(ctx, "dashboard_user_enabled", "", "dashboard_user", userID, map[string]any{"enabled": enabled})
	return nil
}

// dummyHash is a real Argon2id hash of a value nobody will choose, used to equalise
// the cost of a login against an unknown username.
//
// It is computed once at init rather than per attempt: the work factor is fixed,
// so re-deriving it per request would add cost without adding uniformity.
var dummyHash = func() string {
	hash, err := HashPassword("synapass-nonexistent-account-placeholder")
	if err != nil {
		// HashPassword only fails if the system CSPRNG fails, in which case
		// everything else is already broken. A fixed, valid-format string keeps the
		// package usable and still costs the same to verify.
		return "$argon2id$v=19$m=19456,t=2,p=2$Y2Fyb2Jyb290ZXItc2FsdA$0000000000000000000000000000000000000000000000000000000000000000"
	}
	return hash
}()

func (s *Service) audit(ctx context.Context, event, actor, resource, resourceID string, details map[string]any) {
	if s.opts.Audit == nil {
		return
	}
	s.opts.Audit.Record(ctx, event, actor, resource, resourceID, details)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// Describe renders the lockout policy for the setup and login screens, so the
// interface can state the rules instead of leaving an operator to guess.
func (s *Service) Describe() Description {
	return Description{
		MinPasswordLength: s.opts.PasswordRules.MinLength,
		MaxPasswordLength: s.opts.PasswordRules.MaxLength,
		MaxFailedAttempts: s.opts.MaxFailedAttempts,
		SessionMinutes:    int(s.opts.SessionTTL.Minutes()),
	}
}

// Description is the publicly safe policy summary.
type Description struct {
	MinPasswordLength int `json:"min_password_length"`
	MaxPasswordLength int `json:"max_password_length"`
	MaxFailedAttempts int `json:"max_failed_attempts"`
	SessionMinutes    int `json:"session_minutes"`
}
