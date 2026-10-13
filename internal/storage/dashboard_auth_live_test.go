package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shadowsafin/synapass/internal/domain"
)

func mustPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestLiveDashboardUserRepository exercises the repository against a real
// database.
//
// It exists because this layer is otherwise only reached through HTTP, and a
// parameter-encoding mistake there fails quietly: the service logs a warning and
// returns the correct error to the client, so every unit test stays green while
// the brute-force lockout never actually engages. That is precisely the failure
// this caught once already.
func TestLiveDashboardUserRepository(t *testing.T) {
	dsn := os.Getenv("SYNAPASS_TEST_DSN")
	if dsn == "" {
		t.Skip("SYNAPASS_TEST_DSN is not set")
	}

	pool := mustPool(t, dsn)
	repo := NewDashboardUserRepository(pool, nil)
	ctx := context.Background()

	name := "livetest" + time.Now().Format("150405000")
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM dashboard_users WHERE username = $1`, name)
	}()

	// A syntactically valid Argon2id string; the repository never parses it.
	const hash = "$argon2id$v=19$m=19456,t=2,p=2$c2Fyc3RvdC1zYWx0MDA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGE"

	user, err := repo.Create(ctx, &domain.DashboardUser{
		ID: domain.NewID(), Username: name, PasswordHash: hash, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Below the threshold there is no lock to set, so the statement has to cope
	// with a NULL lock instant while still stamping updated_at. This is the path
	// where sharing one parameter for both silently violated the NOT NULL
	// constraint on updated_at.
	if err := repo.RecordLoginFailure(ctx, user.ID, time.Now().UTC(), 5, nil); err != nil {
		t.Fatalf("RecordLoginFailure below threshold: %v (cause: %v)", err, errors.Unwrap(err))
	}
	below, err := repo.ByUsername(ctx, name)
	if err != nil {
		t.Fatalf("ByUsername after a sub-threshold failure: %v", err)
	}
	if below.LockedUntil != nil {
		t.Error("a failure below the threshold must not lock the account")
	}

	// The statement the brute-force protection depends on.
	for attempt := 1; attempt <= 3; attempt++ {
		lockUntil := time.Now().UTC().Add(time.Duration(attempt) * time.Second)
		if err := repo.RecordLoginFailure(ctx, user.ID, time.Now().UTC(), 2, &lockUntil); err != nil {
			t.Fatalf("RecordLoginFailure(attempt %d): %v (cause: %v)", attempt, err, errors.Unwrap(err))
		}
	}

	got, err := repo.ByUsername(ctx, name)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	// One sub-threshold failure above, then three that cross it.
	if got.FailedAttempts != 4 {
		t.Errorf("failed_attempts = %d, want 4", got.FailedAttempts)
	}
	if got.LockedUntil == nil {
		t.Fatal("locked_until was never set, so the lockout would never engage")
	}
	if !got.Locked(time.Now().UTC()) {
		t.Error("Locked reported false while locked_until is in the future")
	}

	if err := repo.RecordLoginSuccess(ctx, user.ID, time.Now().UTC()); err != nil {
		t.Fatalf("RecordLoginSuccess: %v", err)
	}
	got, err = repo.ByUsername(ctx, name)
	if err != nil {
		t.Fatalf("ByUsername after success: %v", err)
	}
	if got.FailedAttempts != 0 || got.LockedUntil != nil {
		t.Errorf("a successful login must clear the counter and the lock: attempts=%d locked=%v",
			got.FailedAttempts, got.LockedUntil)
	}

	if _, err := repo.Create(ctx, &domain.DashboardUser{
		ID: domain.NewID(), Username: name, PasswordHash: hash, Enabled: true,
	}); err == nil {
		t.Error("a duplicate username must be rejected as a conflict, not accepted")
	}

	sessions := NewDashboardSessionRepository(pool, nil)
	created, err := sessions.Create(ctx, &domain.DashboardSession{
		ID: domain.NewID(), UserID: user.ID, TokenHash: "live-token-hash",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Sessions.Create: %v", err)
	}
	if err := sessions.Revoke(ctx, created.TokenHash, time.Now().UTC()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	revoked, err := sessions.ByTokenHash(ctx, created.TokenHash)
	if err != nil {
		// Reaching this line at all is the point: logout marks a session revoked
		// rather than deleting it, so the record of who was signed in survives.
		t.Fatalf("a revoked session must remain readable: %v", err)
	}
	if !revoked.Revoked() {
		t.Error("revoked_at was not set")
	}
	if revoked.Active(time.Now().UTC()) {
		t.Error("a revoked session must not be active")
	}
	_, _ = pool.Exec(ctx, `DELETE FROM dashboard_sessions WHERE user_id = $1`, user.ID)
}
