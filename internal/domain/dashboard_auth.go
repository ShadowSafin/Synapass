package domain

import "time"

// Dashboard operator identity and the sessions it signs in.
//
// Distinct from APIKey, which is a machine credential. An API key is
// high-entropy CSPRNG material verified by digest lookup; a dashboard user is a
// human who chose a password, so the password is stretched with Argon2id and the
// account is lockable. Conflating the two would either make logins slow or make
// keys brute-forceable.

// DashboardUser is a human operator account for the console.
type DashboardUser struct {
	ID string `json:"id"`
	// Username is stored and compared lowercased, so login is case-insensitive.
	Username string `json:"username"`
	// PasswordHash is Argon2id in PHC string format. It never leaves the process.
	PasswordHash string `json:"-"`
	Enabled      bool   `json:"enabled"`
	// FailedAttempts counts consecutive failures and resets on success. It exists
	// so a lockout is honest about why it happened.
	FailedAttempts int        `json:"-"`
	LockedUntil    *time.Time `json:"-"`
	LastLoginAt    *time.Time `json:"last_login_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Locked reports whether the account is inside its lockout window.
func (u *DashboardUser) Locked(now time.Time) bool {
	return u.LockedUntil != nil && u.LockedUntil.After(now)
}

// DashboardSession is one signed-in browser session.
type DashboardSession struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	// TokenHash is the SHA-256 of the session token. The token itself is only
	// ever in the client cookie.
	TokenHash  string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"-"`
	IP         string     `json:"-"`
	UserAgent  string     `json:"-"`
}

// Expired reports whether the session is past its absolute lifetime.
func (s *DashboardSession) Expired(now time.Time) bool {
	return !s.ExpiresAt.After(now)
}

// Revoked reports whether the session was explicitly invalidated.
func (s *DashboardSession) Revoked() bool {
	return s.RevokedAt != nil
}

// Active reports whether the session may still authenticate a request.
func (s *DashboardSession) Active(now time.Time) bool {
	return !s.Revoked() && !s.Expired(now)
}
