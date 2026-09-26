package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// readLockState returns the stored failed_attempts and locked_until for a user.
func readLockState(t *testing.T, central *sql.DB, username string) (int, sql.NullString) {
	t.Helper()
	var failed int
	var locked sql.NullString
	if err := central.QueryRow(
		`SELECT failed_attempts, locked_until FROM "USER" WHERE username = ?`, username,
	).Scan(&failed, &locked); err != nil {
		t.Fatalf("read lock state: %v", err)
	}
	return failed, locked
}

// TestWrongPasswordIncrementsCounter asserts each wrong-password login bumps the
// consecutive-failure counter without locking before the threshold (Req 3.3).
func TestWrongPasswordIncrementsCounter(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2")

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	svc := NewAuthService(conns).withClock(func() time.Time { return now })

	for i := 1; i <= 4; i++ {
		if _, err := svc.Login(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: expected ErrInvalidCredentials, got %v", i, err)
		}
		failed, locked := readLockState(t, central, "alice")
		if failed != i {
			t.Fatalf("after %d failures, failed_attempts = %d, want %d", i, failed, i)
		}
		if locked.Valid {
			t.Fatalf("account should not be locked after %d failures, locked_until = %q", i, locked.String)
		}
	}
}

// TestFifthConsecutiveFailureLocks asserts the 5th consecutive failure sets a
// lockout window of exactly 15 minutes from now (Req 3.3), and that a further
// attempt during the window is refused as locked regardless of correctness.
func TestFifthConsecutiveFailureLocks(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2")

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	svc := NewAuthService(conns).withClock(func() time.Time { return now })

	// Four failures: not yet locked.
	for i := 0; i < 4; i++ {
		if _, err := svc.Login(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: expected ErrInvalidCredentials, got %v", i+1, err)
		}
	}
	if _, locked := readLockState(t, central, "alice"); locked.Valid {
		t.Fatal("account must not be locked before the 5th failure")
	}

	// Fifth failure: still invalid-credentials, but the lock is now armed.
	if _, err := svc.Login(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("5th attempt: expected ErrInvalidCredentials, got %v", err)
	}
	failed, locked := readLockState(t, central, "alice")
	if failed != lockoutThreshold {
		t.Fatalf("failed_attempts = %d, want %d", failed, lockoutThreshold)
	}
	if !locked.Valid {
		t.Fatal("account must be locked after the 5th consecutive failure")
	}
	until, err := time.Parse(time.RFC3339Nano, locked.String)
	if err != nil {
		t.Fatalf("parse locked_until %q: %v", locked.String, err)
	}
	if want := now.Add(lockoutDuration); !until.Equal(want) {
		t.Fatalf("locked_until = %s, want %s (now + 15m)", until, want)
	}

	// A subsequent attempt during the window is refused as locked, even with
	// the correct password.
	if _, err := svc.Login(context.Background(), "alice", "hunter2"); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("during lockout expected ErrAccountLocked, got %v", err)
	}
}

// TestSuccessResetsCounterMidStreak asserts a correct password before the
// threshold clears the accumulated failure count so the run starts over
// (Req 3.3 reset-on-success).
func TestSuccessResetsCounterMidStreak(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2")

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	svc := NewAuthService(conns).withClock(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if _, err := svc.Login(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	if failed, _ := readLockState(t, central, "alice"); failed != 3 {
		t.Fatalf("failed_attempts = %d, want 3", failed)
	}

	if _, err := svc.Login(context.Background(), "alice", "hunter2"); err != nil {
		t.Fatalf("valid login should succeed: %v", err)
	}
	failed, locked := readLockState(t, central, "alice")
	if failed != 0 {
		t.Fatalf("failed_attempts should reset to 0 on success, got %d", failed)
	}
	if locked.Valid {
		t.Fatalf("locked_until should be clear after success, got %q", locked.String)
	}

	// After a reset, it takes a fresh run of failures to lock again — four
	// more failures still do not lock.
	for i := 0; i < 4; i++ {
		if _, err := svc.Login(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("post-reset failure %d: %v", i+1, err)
		}
	}
	if _, locked := readLockState(t, central, "alice"); locked.Valid {
		t.Fatal("four failures after a reset must not lock the account")
	}
}
