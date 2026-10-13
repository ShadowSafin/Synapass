package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Dashboard operator persistence.
//
// Two repositories rather than one, because their access patterns have nothing in
// common: users are looked up by username once per login and otherwise rarely
// touched, while sessions are read on every authenticated page load and written
// only at sign-in and sign-out.

// pgUniqueViolation is the SQLSTATE for a unique constraint failure. It is the
// signal that first-run setup lost a race, which is a normal outcome rather than
// an error: two operators opening /setup at once must produce one winner.
const pgUniqueViolation = "23505"

const dashboardUserColumns = `id, username, password_hash, enabled, failed_attempts,
	locked_until, last_login_at, password_changed_at, created_at, updated_at`

// DashboardUserRepository stores human operator accounts.
type DashboardUserRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewDashboardUserRepository constructs the store.
func NewDashboardUserRepository(pool *pgxpool.Pool, logger *slog.Logger) *DashboardUserRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &DashboardUserRepository{pool: pool, logger: logger}
}

// ErrUsernameTaken reports a username that already exists.
var ErrUsernameTaken = domain.NewError(domain.ErrCodeInvalidRequest, "that username is already taken")

// Create inserts a new operator account.
//
// A unique violation is translated into ErrUsernameTaken rather than a 500,
// because it is the expected outcome of two concurrent first-run submissions and
// the caller must be able to report it as such.
func (r *DashboardUserRepository) Create(ctx context.Context, u *domain.DashboardUser) (*domain.DashboardUser, error) {
	if u.ID == "" {
		u.ID = domain.NewID()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}

	row := r.pool.QueryRow(ctx, `
		INSERT INTO dashboard_users (id, username, password_hash, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		RETURNING `+dashboardUserColumns,
		u.ID, u.Username, u.PasswordHash, u.Enabled, u.CreatedAt)

	out, err := scanDashboardUser(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errorsAs(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, ErrUsernameTaken
		}
		return nil, wrapDBError("create dashboard user", err)
	}
	return out, nil
}

// Count returns how many operator accounts exist.
func (r *DashboardUserRepository) Count(ctx context.Context) (int, error) {
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM dashboard_users`).Scan(&count); err != nil {
		return 0, wrapDBError("count dashboard users", err)
	}
	return count, nil
}

// ByUsername looks an account up for login.
//
// The username is matched exactly because the service normalizes to lowercase
// before calling, and the stored value is already lowercase.
func (r *DashboardUserRepository) ByUsername(ctx context.Context, username string) (*domain.DashboardUser, error) {
	return r.get(ctx,
		`SELECT `+dashboardUserColumns+` FROM dashboard_users WHERE username = $1`,
		username, "dashboard user not found")
}

// ByID looks an account up by identifier.
func (r *DashboardUserRepository) ByID(ctx context.Context, id string) (*domain.DashboardUser, error) {
	return r.get(ctx,
		`SELECT `+dashboardUserColumns+` FROM dashboard_users WHERE id = $1`,
		id, "dashboard user not found")
}

func (r *DashboardUserRepository) get(ctx context.Context, query, arg, missing string) (*domain.DashboardUser, error) {
	out, err := scanDashboardUser(r.pool.QueryRow(ctx, query, arg))
	if isNotFound(err) {
		return nil, domain.NewError(domain.ErrCodeNotFound, missing)
	}
	if err != nil {
		return nil, wrapDBError("read dashboard user", err)
	}
	return out, nil
}

// RecordLoginSuccess clears the failure counter and stamps the login time.
func (r *DashboardUserRepository) RecordLoginSuccess(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE dashboard_users
		   SET failed_attempts = 0, locked_until = NULL, last_login_at = $2, updated_at = $2
		 WHERE id = $1`,
		id, at)
	if err != nil {
		return wrapDBError("record dashboard login success", err)
	}
	return nil
}

// RecordLoginFailure increments the failure counter and applies a lockout when
// the threshold is reached.
//
// The counter is incremented and evaluated in one statement rather than
// read-modify-write in Go, so two concurrent attempts against the same account
// cannot both read the same count and both decide it was the first failure.
//
// lockUntil is passed as a finished timestamp rather than as a duration to add.
// Doing the arithmetic in SQL makes the CASE mix timestamptz with interval, which
// Postgres will not resolve for a typed parameter, and it would put the backoff
// policy in two places. A nil lockUntil leaves any existing lock untouched.
func (r *DashboardUserRepository) RecordLoginFailure(
	ctx context.Context, id string, at time.Time, threshold int, lockUntil *time.Time,
) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE dashboard_users
		   SET failed_attempts = failed_attempts + 1,
		       locked_until = CASE
		           WHEN failed_attempts + 1 >= $2 THEN $3
		           ELSE locked_until
		       END,
		       updated_at = $4
		 WHERE id = $1`,
		id, threshold, lockUntil, at)
	if err != nil {
		return wrapDBError("record dashboard login failure", err)
	}
	return nil
}

// SetPassword replaces the stored hash and clears any lockout.
func (r *DashboardUserRepository) SetPassword(ctx context.Context, id, hash string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE dashboard_users
		   SET password_hash = $2, password_changed_at = $3, updated_at = $3,
		       failed_attempts = 0, locked_until = NULL
		 WHERE id = $1`,
		id, hash, at)
	if err != nil {
		return wrapDBError("update dashboard password", err)
	}
	return nil
}

// SetEnabled enables or disables an account.
func (r *DashboardUserRepository) SetEnabled(ctx context.Context, id string, enabled bool, at time.Time) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE dashboard_users SET enabled = $2, updated_at = $3 WHERE id = $1`,
		id, enabled, at)
	if err != nil {
		return wrapDBError("update dashboard user", err)
	}
	return nil
}

// List returns every operator account, oldest first.
func (r *DashboardUserRepository) List(ctx context.Context) ([]domain.DashboardUser, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+dashboardUserColumns+` FROM dashboard_users ORDER BY created_at ASC`)
	if err != nil {
		return nil, wrapDBError("list dashboard users", err)
	}
	defer rows.Close()

	var out []domain.DashboardUser
	for rows.Next() {
		user, err := scanDashboardUser(rows)
		if err != nil {
			return nil, wrapDBError("scan dashboard user", err)
		}
		out = append(out, *user)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

const dashboardSessionColumns = `id, user_id::text, token_hash, created_at,
	last_seen_at, expires_at, revoked_at, ip, user_agent`

// DashboardSessionRepository stores browser sessions.
type DashboardSessionRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewDashboardSessionRepository constructs the store.
func NewDashboardSessionRepository(pool *pgxpool.Pool, logger *slog.Logger) *DashboardSessionRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &DashboardSessionRepository{pool: pool, logger: logger}
}

// Create inserts a session.
func (r *DashboardSessionRepository) Create(ctx context.Context, s *domain.DashboardSession) (*domain.DashboardSession, error) {
	if s.ID == "" {
		s.ID = domain.NewID()
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.LastSeenAt = now

	row := r.pool.QueryRow(ctx, `
		INSERT INTO dashboard_sessions (id, user_id, token_hash, created_at, last_seen_at,
			expires_at, ip, user_agent)
		VALUES ($1,$2,$3,$4,$4,$5,$6,$7)
		RETURNING `+dashboardSessionColumns,
		s.ID, s.UserID, s.TokenHash, s.CreatedAt, s.ExpiresAt, s.IP, s.UserAgent)

	out, err := scanDashboardSession(row)
	if err != nil {
		return nil, wrapDBError("create dashboard session", err)
	}
	return out, nil
}

// ByTokenHash resolves a session for an incoming request.
//
// A revoked or expired row is still returned rather than filtered out here: the
// caller needs to tell "unknown token" (401, retry login) apart from "expired"
// (clear the cookie), and collapsing both makes the client loop on a stale cookie.
func (r *DashboardSessionRepository) ByTokenHash(ctx context.Context, tokenHash string) (*domain.DashboardSession, error) {
	out, err := scanDashboardSession(r.pool.QueryRow(ctx,
		`SELECT `+dashboardSessionColumns+` FROM dashboard_sessions WHERE token_hash = $1`, tokenHash))
	if isNotFound(err) {
		return nil, domain.NewError(domain.ErrCodeNotFound, "session not found")
	}
	if err != nil {
		return nil, wrapDBError("read dashboard session", err)
	}
	return out, nil
}

// Touch updates the last-seen stamp used for idle expiry.
//
// Best effort by design: a failure to write telemetry about a session must never
// fail the request that the session just authorised.
func (r *DashboardSessionRepository) Touch(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE dashboard_sessions SET last_seen_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return wrapDBError("touch dashboard session", err)
	}
	return nil
}

// Revoke invalidates one session by its token hash.
func (r *DashboardSessionRepository) Revoke(ctx context.Context, tokenHash string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE dashboard_sessions SET revoked_at = $2
		 WHERE token_hash = $1 AND revoked_at IS NULL`,
		tokenHash, at)
	if err != nil {
		return wrapDBError("revoke dashboard session", err)
	}
	return nil
}

// RevokeAllForUser invalidates every live session for an account.
//
// Used when the password changes, so that changing a password actually evicts
// whoever else was holding a session.
func (r *DashboardSessionRepository) RevokeAllForUser(ctx context.Context, userID string, at time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE dashboard_sessions SET revoked_at = $2
		 WHERE user_id = $1 AND revoked_at IS NULL`,
		userID, at)
	if err != nil {
		return 0, wrapDBError("revoke dashboard sessions", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteExpired removes sessions that expired before the cutoff.
func (r *DashboardSessionRepository) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM dashboard_sessions WHERE expires_at < $1`, before)
	if err != nil {
		return 0, wrapDBError("delete expired dashboard sessions", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Scanners
// ---------------------------------------------------------------------------

// rowScanner is satisfied by both pgx.Row and pgx.Rows, so one scanner serves the
// single-row and multi-row reads.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanDashboardUser(row rowScanner) (*domain.DashboardUser, error) {
	var (
		u          domain.DashboardUser
		lockedTill *time.Time
		lastLogin  *time.Time
		passwordAt time.Time
	)
	err := row.Scan(
		&u.ID, &u.Username, &u.PasswordHash, &u.Enabled, &u.FailedAttempts,
		&lockedTill, &lastLogin, &passwordAt, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	u.LockedUntil = lockedTill
	u.LastLoginAt = lastLogin
	return &u, nil
}

func scanDashboardSession(row rowScanner) (*domain.DashboardSession, error) {
	var (
		s       domain.DashboardSession
		revoked *time.Time
	)
	err := row.Scan(
		&s.ID, &s.UserID, &s.TokenHash, &s.CreatedAt,
		&s.LastSeenAt, &s.ExpiresAt, &revoked, &s.IP, &s.UserAgent,
	)
	if err != nil {
		return nil, err
	}
	s.RevokedAt = revoked
	return &s, nil
}

// errorsAs unwraps to a driver error type without importing errors for a single
// assertion.
func errorsAs(err error, target **pgconn.PgError) bool {
	for err != nil {
		if pgErr, ok := err.(*pgconn.PgError); ok {
			*target = pgErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
