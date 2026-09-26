package service

// Unit tests for the second-factor login integration (Req 33.4, 33.5, 33.6,
// 33.9). They drive AuthService.LoginWithFactor directly over a real in-memory
// Central_Directory (newTestConns/seedTenant/seedUser from auth_test.go), with
// a real TOTP computed from the stored secret via the same library the service
// verifies with and real Argon2id-hashed Recovery_Codes — no fakes.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// enrolUserWithSecret marks a user enrolled and stores a fresh TOTP_Secret,
// returning the base32 secret so the test can compute valid codes from it.
func enrolUserWithSecret(t *testing.T, central *sql.DB, userID int64) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Influence", AccountName: "test"})
	if err != nil {
		t.Fatalf("generate totp key: %v", err)
	}
	secret := key.Secret()
	if _, err := central.Exec(
		`UPDATE "USER" SET two_factor_enrolled = 1, two_factor_secret = ? WHERE id = ?`,
		secret, userID,
	); err != nil {
		t.Fatalf("enrol user with secret: %v", err)
	}
	return secret
}

// issueRecoveryCode stores one Argon2id-hashed Recovery_Code for the user and
// returns the plaintext.
func issueRecoveryCode(t *testing.T, central *sql.DB, userID int64, plaintext string) {
	t.Helper()
	hash, err := HashPassword(plaintext)
	if err != nil {
		t.Fatalf("hash recovery code: %v", err)
	}
	if _, err := central.Exec(
		`INSERT INTO "RECOVERY_CODE"(user_id, code_hash) VALUES(?, ?)`,
		userID, hash,
	); err != nil {
		t.Fatalf("insert recovery code: %v", err)
	}
}

// TestLoginWithValidTOTPEstablishesActiveSession verifies an enrolled user who
// supplies a valid TOTP gets an 'active' session, and the consumed step is
// recorded in last_totp_step (Req 33.4, 33.9).
func TestLoginWithValidTOTPEstablishesActiveSession(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	secret := enrolUserWithSecret(t, central, userID)

	now := time.Date(2024, 5, 1, 12, 0, 30, 0, time.UTC)
	code, err := totp.GenerateCode(secret, now)
	if err != nil {
		t.Fatalf("generate totp: %v", err)
	}

	svc := NewAuthService(conns).withClock(func() time.Time { return now })
	sess, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", code)
	if err != nil {
		t.Fatalf("login with valid totp: %v", err)
	}
	if sess.State != SessionStateActive {
		t.Fatalf("session state = %q, want %q", sess.State, SessionStateActive)
	}
	if sess.Token == "" {
		t.Fatal("active session must carry a token")
	}

	var lastStep sql.NullInt64
	if err := central.QueryRow(
		`SELECT last_totp_step FROM "USER" WHERE id = ?`, userID,
	).Scan(&lastStep); err != nil {
		t.Fatalf("read last_totp_step: %v", err)
	}
	if !lastStep.Valid {
		t.Fatal("last_totp_step should be recorded after a consumed TOTP")
	}
	if want := now.Unix() / totpStepSeconds; lastStep.Int64 != want {
		t.Fatalf("last_totp_step = %d, want %d", lastStep.Int64, want)
	}
}

// TestLoginEnrolledNoCodeRequiresSecondFactor verifies an enrolled user with a
// correct password but no code gets ErrSecondFactorRequired and NO session is
// created (Req 33.4).
func TestLoginEnrolledNoCodeRequiresSecondFactor(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	enrolUserWithSecret(t, central, userID)

	svc := NewAuthService(conns)
	_, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", "")
	if !errors.Is(err, ErrSecondFactorRequired) {
		t.Fatalf("expected ErrSecondFactorRequired, got %v", err)
	}

	var count int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "SESSION"`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("second-factor-required must create no session, found %d", count)
	}
}

// TestLoginInvalidTOTPFeedsLockout verifies a wrong second factor denies access
// and increments the SAME failed_attempts counter password failures use, and
// that the 5th consecutive failure arms the lockout (Req 33.5, reusing Req 3.3).
func TestLoginInvalidTOTPFeedsLockout(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	secret := enrolUserWithSecret(t, central, userID)

	now := time.Date(2024, 5, 1, 12, 0, 30, 0, time.UTC)
	// A code that is definitely not valid for the current window.
	valid, err := totp.GenerateCode(secret, now)
	if err != nil {
		t.Fatalf("generate totp: %v", err)
	}
	bad := "000000"
	if valid == bad {
		bad = "000001"
	}

	svc := NewAuthService(conns).withClock(func() time.Time { return now })

	for attempt := 1; attempt <= lockoutThreshold; attempt++ {
		_, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", bad)
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: expected ErrInvalidCredentials, got %v", attempt, err)
		}
		failed, locked := readLockState(t, central, "alice")
		if failed != attempt {
			t.Fatalf("after attempt %d: failed_attempts = %d, want %d", attempt, failed, attempt)
		}
		wantLocked := attempt >= lockoutThreshold
		if locked.Valid != wantLocked {
			t.Fatalf("after attempt %d: locked=%v, want %v", attempt, locked.Valid, wantLocked)
		}
	}

	// Even a valid code is now refused while the lock is active (Req 3.3).
	if _, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", valid); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("during lockout expected ErrAccountLocked, got %v", err)
	}
}

// TestLoginTOTPReplayRejected verifies a TOTP consumed once cannot be reused
// within its window: the same code at the same step is rejected on the second
// use because its step is not newer than last_totp_step (Req 33.9).
func TestLoginTOTPReplayRejected(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	secret := enrolUserWithSecret(t, central, userID)

	now := time.Date(2024, 5, 1, 12, 0, 30, 0, time.UTC)
	code, err := totp.GenerateCode(secret, now)
	if err != nil {
		t.Fatalf("generate totp: %v", err)
	}

	svc := NewAuthService(conns).withClock(func() time.Time { return now })

	// First use succeeds.
	if _, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", code); err != nil {
		t.Fatalf("first use of totp: %v", err)
	}
	// Replay at the same step is rejected as an invalid second factor.
	if _, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", code); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("replayed totp: expected ErrInvalidCredentials, got %v", err)
	}
}

// TestLoginWithRecoveryCodeConsumesIt verifies a valid Recovery_Code
// establishes an 'active' session and is marked used so it cannot be reused
// (Req 33.6).
func TestLoginWithRecoveryCodeConsumesIt(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	enrolUserWithSecret(t, central, userID)
	const recovery = "BACKUPCODE123456"
	issueRecoveryCode(t, central, userID, recovery)

	svc := NewAuthService(conns)
	sess, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", recovery)
	if err != nil {
		t.Fatalf("login with recovery code: %v", err)
	}
	if sess.State != SessionStateActive {
		t.Fatalf("session state = %q, want %q", sess.State, SessionStateActive)
	}

	var used int
	if err := central.QueryRow(
		`SELECT used FROM "RECOVERY_CODE" WHERE user_id = ?`, userID,
	).Scan(&used); err != nil {
		t.Fatalf("read recovery code used flag: %v", err)
	}
	if used != 1 {
		t.Fatalf("recovery code used = %d, want 1 after redemption", used)
	}
}

// TestLoginReusedRecoveryCodeRejected verifies a Recovery_Code redeemed once is
// refused on a second use, feeding the lockout like any invalid second factor
// (Req 33.6).
func TestLoginReusedRecoveryCodeRejected(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	enrolUserWithSecret(t, central, userID)
	const recovery = "BACKUPCODE123456"
	issueRecoveryCode(t, central, userID, recovery)

	svc := NewAuthService(conns)
	if _, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", recovery); err != nil {
		t.Fatalf("first recovery-code use: %v", err)
	}
	if _, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", recovery); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("reused recovery code: expected ErrInvalidCredentials, got %v", err)
	}
}

// TestLoginWithFactorMustEnrolWhenRequiredAndUnenrolled verifies that through
// the second-factor login path, an unenrolled user in a required-policy tenant
// still gets a 'must_enrol' restricted session (Req 33.7, 33.8).
func TestLoginWithFactorMustEnrolWhenRequiredAndUnenrolled(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")

	twoFactor := NewTwoFactorService(conns)
	if err := twoFactor.SetTenantPolicy(context.Background(), rcFor(userID, tenantID), true); err != nil {
		t.Fatalf("set policy required: %v", err)
	}

	svc := NewAuthService(conns)
	sess, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.State != SessionStateMustEnrol {
		t.Fatalf("session state = %q, want %q", sess.State, SessionStateMustEnrol)
	}
}

// TestLoginWithFactorUnenrolledOptionalActive verifies an unenrolled user with
// no policy still reaches a normal 'active' session with no code (Req 33.7).
func TestLoginWithFactorUnenrolledOptionalActive(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2xY!")

	svc := NewAuthService(conns)
	sess, err := svc.LoginWithFactor(context.Background(), "alice", "hunter2xY!", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.State != SessionStateActive {
		t.Fatalf("session state = %q, want %q", sess.State, SessionStateActive)
	}
}

// TestLoginWithFactorWrongPasswordDenied verifies a wrong password on the
// second-factor path is still ErrInvalidCredentials and creates no session,
// even when a code is supplied (Req 3.2).
func TestLoginWithFactorWrongPasswordDenied(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	secret := enrolUserWithSecret(t, central, userID)
	code, _ := totp.GenerateCode(secret, time.Now())

	svc := NewAuthService(conns)
	if _, err := svc.LoginWithFactor(context.Background(), "alice", "wrongpass", code); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	var count int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "SESSION"`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("wrong password must create no session, found %d", count)
	}
}
