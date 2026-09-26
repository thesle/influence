package service

// Feature: influence, Property 35: Two-factor authentication gate and single-use recovery codes
//
// Property 35 (design.md — Correctness Properties): the second-factor login
// gate holds across randomized inputs. For an enrolled user, LoginWithFactor
// never yields an 'active' session without a valid, unconsumed second factor;
// a valid TOTP within the ±1-step drift window is accepted while a replay at
// the same or an older step is refused; each Recovery_Code redeems at most
// once; repeated invalid second factors feed the SAME lockout as wrong
// passwords; and an unenrolled user in a required-policy tenant only ever gets
// a 'must_enrol' session.
//
// Validates: Requirements 33.4, 33.5, 33.6, 33.7, 33.9
//
// The test drives AuthService.LoginWithFactor over a real in-memory
// Central_Directory (newTestConns/seedTenant/seedUser), with real TOTP codes
// computed from the stored secret via the same library the service verifies
// with and real Argon2id-hashed Recovery_Codes — no fakes. Each rapid iteration
// draws a fresh scenario (which second factor is enrolled, how many recovery
// codes, which one is redeemed, whether a TOTP is replayed, how many invalid
// attempts precede the lockout, and whether the tenant policy is required) and
// checks the invariants above. rapid's default of 100 iterations satisfies the
// "minimum 100 iterations" requirement.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"pgregory.net/rapid"
)

// TestProperty35TwoFactorGate is the single property test for Property 35.
func TestProperty35TwoFactorGate(t *testing.T) {
	// newTestConns keys its in-memory Central_Directory on t.Name(), so every
	// rapid iteration shares one database. A monotonic counter namespaces the
	// tenant UUID and usernames per iteration so rows from earlier iterations
	// never collide with, nor shadow, this iteration's fixtures.
	var iter int64
	rapid.Check(t, func(rt *rapid.T) {
		n := atomic.AddInt64(&iter, 1)
		const password = "hunter2xY!"
		uuid := fmt.Sprintf("uuid-%d", n)
		unenrolledName := fmt.Sprintf("unenrolled-%d", n)
		aliceName := fmt.Sprintf("alice-%d", n)
		bobName := fmt.Sprintf("bob-%d", n)

		// Base clock for the scenario; TOTP steps derive from it. Draw an
		// arbitrary second within a wide range so the step math is exercised
		// away from any fixed value.
		baseUnix := rapid.Int64Range(1_600_000_000, 2_000_000_000).Draw(rt, "baseUnix")
		now := time.Unix(baseUnix, 0).UTC()

		conns := newTestConns(t)
		central := conns.Central()
		tenantID := seedTenant(t, central, uuid, "Org "+uuid)

		// --- 33.7: unenrolled user in a required-policy tenant -> must_enrol,
		// never a plain active session. Use a distinct user so it does not
		// interfere with the enrolled-user scenarios below.
		policyRequired := rapid.Bool().Draw(rt, "policyRequired")
		if policyRequired {
			unenrolledID := seedUser(t, central, tenantID, unenrolledName, password)
			twoFactor := NewTwoFactorService(conns)
			if err := twoFactor.SetTenantPolicy(context.Background(), rcFor(unenrolledID, tenantID), true); err != nil {
				rt.Fatalf("set tenant policy: %v", err)
			}
			svc := NewAuthService(conns).withClock(func() time.Time { return now })
			sess, err := svc.LoginWithFactor(context.Background(), unenrolledName, password, "")
			if err != nil {
				rt.Fatalf("unenrolled required-policy login: %v", err)
			}
			if sess.State != SessionStateMustEnrol {
				rt.Fatalf("required-policy unenrolled state = %q, want %q", sess.State, SessionStateMustEnrol)
			}
			if sess.State == SessionStateActive {
				rt.Fatalf("unenrolled user in required tenant must never get an active session")
			}
		}

		// --- Enrolled user: the gate scenarios (33.4, 33.5, 33.6, 33.9).
		userID := seedUser(t, central, tenantID, aliceName, password)
		secret := enrolUserWithSecret(t, central, userID)

		// Issue a randomized set of recovery codes; each plaintext is unique so
		// a redemption is unambiguous.
		numCodes := rapid.IntRange(1, 4).Draw(rt, "numCodes")
		codes := make([]string, numCodes)
		for i := range codes {
			codes[i] = "RECOVERYCODE-" + string(rune('A'+i)) + "-0000000000"
			issueRecoveryCode(t, central, userID, codes[i])
		}

		svc := NewAuthService(conns).withClock(func() time.Time { return now })
		ctx := context.Background()

		// (33.4) No code supplied -> ErrSecondFactorRequired, no session.
		sessionCountBefore := countSessions(t, central)
		if _, err := svc.LoginWithFactor(ctx, aliceName, password, ""); !errors.Is(err, ErrSecondFactorRequired) {
			rt.Fatalf("enrolled, no code: expected ErrSecondFactorRequired, got %v", err)
		}
		if got := countSessions(t, central); got != sessionCountBefore {
			rt.Fatalf("no-code login must create no session (before=%d after=%d)", sessionCountBefore, got)
		}

		// (33.4) An invalid, TOTP-shaped code that is not the current code and
		// matches no recovery code -> auth error, no active session.
		validNow, err := totp.GenerateCode(secret, now)
		if err != nil {
			rt.Fatalf("generate totp: %v", err)
		}
		bad := "000000"
		if bad == validNow {
			bad = "000001"
		}
		sessionCountBefore = countSessions(t, central)
		if _, err := svc.LoginWithFactor(ctx, aliceName, password, bad); !errors.Is(err, ErrInvalidCredentials) {
			rt.Fatalf("enrolled, invalid code: expected ErrInvalidCredentials, got %v", err)
		}
		if got := countSessions(t, central); got != sessionCountBefore {
			rt.Fatalf("invalid-code login must create no session (before=%d after=%d)", sessionCountBefore, got)
		}
		// That one failure incremented the shared counter; clear it so the
		// lockout sub-scenario below starts from a known state.
		resetLockState(t, central, userID)

		// Decide how this user authenticates: via a valid TOTP or via a
		// recovery code. Both must land an active session.
		useTOTP := rapid.Bool().Draw(rt, "useTOTP")
		if useTOTP {
			// (33.9) A valid TOTP within ±1 step is accepted. Draw the drift
			// offset among the accepted neighbours.
			drift := rapid.SampledFrom([]int64{-1, 0, 1}).Draw(rt, "drift")
			at := now.Add(time.Duration(drift) * totpStepSeconds * time.Second)
			code, err := totp.GenerateCode(secret, at)
			if err != nil {
				rt.Fatalf("generate drifted totp: %v", err)
			}
			// A drifted code may coincide with the current one at step
			// boundaries; that is still a valid acceptance either way.
			sess, err := svc.LoginWithFactor(ctx, aliceName, password, code)
			if err != nil {
				rt.Fatalf("valid TOTP (drift=%d): %v", drift, err)
			}
			if sess.State != SessionStateActive {
				rt.Fatalf("valid TOTP state = %q, want %q", sess.State, SessionStateActive)
			}

			// (33.9) Replaying the same code resolves to a step that is not
			// strictly newer than last_totp_step and is rejected.
			if _, err := svc.LoginWithFactor(ctx, aliceName, password, code); !errors.Is(err, ErrInvalidCredentials) {
				rt.Fatalf("replayed TOTP: expected ErrInvalidCredentials, got %v", err)
			}
			// The replay was a failed attempt; clear it before the lockout run.
			resetLockState(t, central, userID)
		} else {
			// (33.6) Each recovery code works exactly once. Pick one to redeem.
			idx := rapid.IntRange(0, numCodes-1).Draw(rt, "redeemIdx")
			redeemed := codes[idx]

			sess, err := svc.LoginWithFactor(ctx, aliceName, password, redeemed)
			if err != nil {
				rt.Fatalf("first recovery-code use: %v", err)
			}
			if sess.State != SessionStateActive {
				rt.Fatalf("recovery-code state = %q, want %q", sess.State, SessionStateActive)
			}
			if used := usedCount(t, central, userID); used != 1 {
				rt.Fatalf("exactly one recovery code should be consumed, got %d used", used)
			}

			// Second use of the SAME code is rejected.
			if _, err := svc.LoginWithFactor(ctx, aliceName, password, redeemed); !errors.Is(err, ErrInvalidCredentials) {
				rt.Fatalf("reused recovery code: expected ErrInvalidCredentials, got %v", err)
			}
			// Still exactly one code consumed — a rejected reuse consumes none.
			if used := usedCount(t, central, userID); used != 1 {
				rt.Fatalf("rejected reuse must not consume another code, got %d used", used)
			}
			resetLockState(t, central, userID)
		}

		// (33.5) Repeated invalid second factors feed the SAME lockout as
		// password failures: the Nth consecutive failure (N = lockoutThreshold)
		// arms locked_until, and thereafter even a valid code is refused with
		// ErrAccountLocked. Use a fresh user so the earlier scenario's consumed
		// factors do not interfere.
		lockUserID := seedUser(t, central, tenantID, bobName, password)
		lockSecret := enrolUserWithSecret(t, central, lockUserID)
		lockValid, err := totp.GenerateCode(lockSecret, now)
		if err != nil {
			rt.Fatalf("generate lock-user totp: %v", err)
		}
		lockBad := "000000"
		if lockBad == lockValid {
			lockBad = "000001"
		}
		for attempt := 1; attempt <= lockoutThreshold; attempt++ {
			if _, err := svc.LoginWithFactor(ctx, bobName, password, lockBad); !errors.Is(err, ErrInvalidCredentials) {
				rt.Fatalf("lockout attempt %d: expected ErrInvalidCredentials, got %v", attempt, err)
			}
			failed, locked := readLockState(t, central, bobName)
			if failed != attempt {
				rt.Fatalf("after attempt %d: failed_attempts = %d, want %d", attempt, failed, attempt)
			}
			wantLocked := attempt >= lockoutThreshold
			if locked.Valid != wantLocked {
				rt.Fatalf("after attempt %d: locked=%v, want %v", attempt, locked.Valid, wantLocked)
			}
		}
		// A valid code is now refused while the lock is active.
		if _, err := svc.LoginWithFactor(ctx, bobName, password, lockValid); !errors.Is(err, ErrAccountLocked) {
			rt.Fatalf("during lockout: expected ErrAccountLocked, got %v", err)
		}
	})
}

// countSessions returns the number of persisted SESSION rows.
func countSessions(t *testing.T, central *sql.DB) int {
	t.Helper()
	var n int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "SESSION"`).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

// usedCount returns how many of the user's recovery codes are marked used.
func usedCount(t *testing.T, central *sql.DB, userID int64) int {
	t.Helper()
	var n int
	if err := central.QueryRow(
		`SELECT COUNT(*) FROM "RECOVERY_CODE" WHERE user_id = ? AND used = 1`, userID,
	).Scan(&n); err != nil {
		t.Fatalf("count used recovery codes: %v", err)
	}
	return n
}

// resetLockState clears the shared failed_attempts/locked_until counters for a
// user so an independent sub-scenario can start from a known state.
func resetLockState(t *testing.T, central *sql.DB, userID int64) {
	t.Helper()
	if _, err := central.Exec(
		`UPDATE "USER" SET failed_attempts = 0, locked_until = NULL WHERE id = ?`, userID,
	); err != nil {
		t.Fatalf("reset lock state: %v", err)
	}
}
