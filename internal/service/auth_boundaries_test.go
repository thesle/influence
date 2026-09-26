package service

// Task 5.4 — focused example/boundary tests for lockout and session bounds
// (Requirements 3.3, 3.5, 3.6).
//
// The counter/lock and session lifecycle already have broad coverage in
// lockout_test.go and session_test.go (from tasks 5.2/5.3). The tests here are
// deliberately narrow and table-driven: each one pins down a single boundary as
// an explicit just-inside-vs-just-past pair so the exact transition point is
// asserted in one place, matching task 5.4's wording ("exactly on the 5th",
// "the 30-minute boundary", "the 24-hour boundary"). They reuse the existing
// helpers (loginAt, svcAt, readLockState, seed*) rather than redefining them.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLockoutTriggerIsExactlyFifthFailure walks the consecutive-failure count
// across the lockout threshold and asserts the lock arms on exactly the 5th
// failure — not the 4th (still unlocked) and not only by the 6th (already
// locked by 5). This nails the exact trigger point in a single contiguous
// sequence (Req 3.3).
func TestLockoutTriggerIsExactlyFifthFailure(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2")

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	svc := NewAuthService(conns).withClock(func() time.Time { return now })

	// The lock must be absent up to and including the 4th failure, and present
	// from the 5th onward. Asserting the state after each attempt makes the
	// off-by-one boundary (4 vs 5) explicit.
	for attempt := 1; attempt <= lockoutThreshold; attempt++ {
		if _, err := svc.Login(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: expected ErrInvalidCredentials, got %v", attempt, err)
		}
		failed, locked := readLockState(t, central, "alice")
		if failed != attempt {
			t.Fatalf("after attempt %d: failed_attempts = %d, want %d", attempt, failed, attempt)
		}
		wantLocked := attempt >= lockoutThreshold // only true on the 5th
		if locked.Valid != wantLocked {
			t.Fatalf("after attempt %d: locked = %v, want %v (lock must arm on exactly the %dth failure)",
				attempt, locked.Valid, wantLocked, lockoutThreshold)
		}
	}
}

// TestIdleBoundaryExact asserts the 30-minute inactivity bound is inclusive:
// exactly 30m of inactivity is still valid, one second past is expired (Req
// 3.6). The two cases share one setup path so the boundary is asserted as a
// just-inside/just-past pair.
func TestIdleBoundaryExact(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		idle    time.Duration
		wantErr bool
	}{
		{"just inside (exactly 30m)", sessionIdleTimeout, false},
		{"just past (30m + 1s)", sessionIdleTimeout + time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conns, sess, _, _ := loginAt(t, start)
			_, err := svcAt(conns, start.Add(tc.idle)).ValidateSession(context.Background(), sess.Token)
			if tc.wantErr {
				if !errors.Is(err, ErrSessionInvalid) {
					t.Fatalf("idle %v: expected ErrSessionInvalid, got %v", tc.idle, err)
				}
			} else if err != nil {
				t.Fatalf("idle %v: expected valid session, got %v", tc.idle, err)
			}
		})
	}
}

// TestAbsoluteBoundaryExact asserts the 24-hour absolute lifetime bound is
// inclusive: a continuously-active session is valid at exactly 24h and expired
// one second past, even though activity keeps sliding last_seen_at (Req 3.6).
// Sliding activity every 20 minutes keeps the idle bound from tripping so the
// absolute ceiling is the only thing under test.
func TestAbsoluteBoundaryExact(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		age     time.Duration
		wantErr bool
	}{
		{"just inside (exactly 24h)", sessionAbsoluteLifetime, false},
		{"just past (24h + 1s)", sessionAbsoluteLifetime + time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conns, sess, _, _ := loginAt(t, start)
			svc := NewAuthService(conns)

			// Keep the session active every 20 minutes up to (but not past) the
			// target age so the 30m idle bound never fires and the absolute
			// bound is what decides validity at the target time.
			for m := 20; time.Duration(m)*time.Minute < tc.age; m += 20 {
				at := start.Add(time.Duration(m) * time.Minute)
				svc.withClock(func() time.Time { return at })
				if _, err := svc.ValidateSession(context.Background(), sess.Token); err != nil {
					t.Fatalf("keep-alive at +%dm should be valid: %v", m, err)
				}
			}

			svc.withClock(func() time.Time { return start.Add(tc.age) })
			_, err := svc.ValidateSession(context.Background(), sess.Token)
			if tc.wantErr {
				if !errors.Is(err, ErrSessionInvalid) {
					t.Fatalf("age %v: expected ErrSessionInvalid, got %v", tc.age, err)
				}
			} else if err != nil {
				t.Fatalf("age %v: expected valid session, got %v", tc.age, err)
			}
		})
	}
}

// TestLogoutInvalidatesWithinValidWindow asserts logout invalidates a session
// that is otherwise still within both expiry bounds: the very next validation
// with the same token fails even though no time bound was exceeded (Req 3.5).
// This isolates the "logout invalidates" effect from any expiry, which the
// broader TestLogoutTerminatesSession does not separate as sharply.
func TestLogoutInvalidatesWithinValidWindow(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, _, _ := loginAt(t, start)

	// One minute after login the session is comfortably valid on both bounds.
	svc := svcAt(conns, start.Add(time.Minute))
	if _, err := svc.ValidateSession(context.Background(), sess.Token); err != nil {
		t.Fatalf("precondition: session should be valid before logout, got %v", err)
	}

	if err := svc.Logout(context.Background(), sess.Token); err != nil {
		t.Fatalf("logout: %v", err)
	}

	// Same instant, same token: logout — not expiry — must invalidate it.
	if _, err := svc.ValidateSession(context.Background(), sess.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("after logout within the valid window: expected ErrSessionInvalid, got %v", err)
	}
}
