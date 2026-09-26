package api

// Integration tests for the second-factor login flow through the unauthenticated
// POST /api/auth/login route, driven end to end via Server.Router() over a real
// Central_Directory (newTestEnv). They prove the handler returns the distinct
// 202 states (second-factor-required, enrolment-required) versus a 200 active
// login, sets a cookie only where a session is actually established, and feeds
// an invalid second factor into the shared lockout (Req 33.4, 33.5, 33.6, 33.9).

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/influence/influence/internal/service"
)

// setLoginablePassword replaces a seeded user's placeholder hash with a real
// Argon2id hash of the given password so the login handler can verify it.
func setLoginablePassword(t *testing.T, e *testEnv, username, password string) {
	t.Helper()
	hash, err := service.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := e.central.Exec(
		`UPDATE "USER" SET password_hash = ? WHERE username = ?`, hash, username,
	); err != nil {
		t.Fatalf("set password: %v", err)
	}
}

// userIDByName reads a user's id from Central.
func userIDByName(t *testing.T, e *testEnv, username string) int64 {
	t.Helper()
	var id int64
	if err := e.central.QueryRow(
		`SELECT id FROM "USER" WHERE username = ?`, username,
	).Scan(&id); err != nil {
		t.Fatalf("lookup user id: %v", err)
	}
	return id
}

// enrolViaSecret marks the user enrolled with a fresh TOTP_Secret and returns
// the base32 secret so the test can compute valid codes.
func enrolViaSecret(t *testing.T, e *testEnv, username string) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Influence", AccountName: username})
	if err != nil {
		t.Fatalf("generate totp key: %v", err)
	}
	if _, err := e.central.Exec(
		`UPDATE "USER" SET two_factor_enrolled = 1, two_factor_secret = ? WHERE username = ?`,
		key.Secret(), username,
	); err != nil {
		t.Fatalf("enrol user: %v", err)
	}
	return key.Secret()
}

// hasSessionCookie reports whether the response set a non-empty session cookie.
func hasSessionCookie(rec *http.Response) bool {
	for _, c := range rec.Cookies() {
		if c.Name == service.SessionCookieName && c.Value != "" && c.MaxAge >= 0 {
			return true
		}
	}
	return false
}

// decodeStatus extracts the "status" field from a JSON body.
func decodeStatus(t *testing.T, body []byte) string {
	t.Helper()
	var v struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode status %q: %v", string(body), err)
	}
	return v.Status
}

// TestLoginNoTwoFactorReturns200 verifies a user with no 2FA enrolled and no
// tenant policy logs in to a 200 'active' session with a cookie (Req 3.1).
func TestLoginNoTwoFactorReturns200(t *testing.T) {
	e := newTestEnv(t)
	setLoginablePassword(t, e, "alice", "hunter2xY!")

	rec := e.do(t, http.MethodPost, "/api/auth/login", "", loginRequest{Username: "alice", Password: "hunter2xY!"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !hasSessionCookie(rec.Result()) {
		t.Fatal("a fully authenticated login must set a session cookie")
	}
}

// TestLoginEnrolledNoCodeReturns202 verifies an enrolled user who supplies no
// code gets 202 second_factor_required and NO session cookie (Req 33.4).
func TestLoginEnrolledNoCodeReturns202(t *testing.T) {
	e := newTestEnv(t)
	setLoginablePassword(t, e, "alice", "hunter2xY!")
	enrolViaSecret(t, e, "alice")

	rec := e.do(t, http.MethodPost, "/api/auth/login", "", loginRequest{Username: "alice", Password: "hunter2xY!"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	if got := decodeStatus(t, rec.Body.Bytes()); got != "second_factor_required" {
		t.Fatalf("status field = %q, want second_factor_required", got)
	}
	if hasSessionCookie(rec.Result()) {
		t.Fatal("second-factor-required must not set a session cookie")
	}
}

// TestLoginEnrolledValidTOTPReturns200 verifies an enrolled user who supplies a
// valid TOTP logs in to a 200 'active' session with a cookie (Req 33.4, 33.9).
func TestLoginEnrolledValidTOTPReturns200(t *testing.T) {
	e := newTestEnv(t)
	setLoginablePassword(t, e, "alice", "hunter2xY!")
	secret := enrolViaSecret(t, e, "alice")

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp: %v", err)
	}

	rec := e.do(t, http.MethodPost, "/api/auth/login", "",
		loginRequest{Username: "alice", Password: "hunter2xY!", Code: code})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !hasSessionCookie(rec.Result()) {
		t.Fatal("a valid second factor must establish a session cookie")
	}
}

// TestLoginEnrolledInvalidTOTPReturns401 verifies a wrong second factor is a
// generic 401 (which factor failed is not leaked) and sets no cookie (Req 33.5).
func TestLoginEnrolledInvalidTOTPReturns401(t *testing.T) {
	e := newTestEnv(t)
	setLoginablePassword(t, e, "alice", "hunter2xY!")
	secret := enrolViaSecret(t, e, "alice")

	valid, _ := totp.GenerateCode(secret, time.Now())
	bad := "000000"
	if valid == bad {
		bad = "000001"
	}

	rec := e.do(t, http.MethodPost, "/api/auth/login", "",
		loginRequest{Username: "alice", Password: "hunter2xY!", Code: bad})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeUnauthenticated {
		t.Fatalf("error code = %q, want %q", code, codeUnauthenticated)
	}
	if hasSessionCookie(rec.Result()) {
		t.Fatal("an invalid second factor must not set a session cookie")
	}
}

// TestLoginEnrolmentRequiredReturns202 verifies an unenrolled user in a tenant
// whose policy requires 2FA gets 202 enrolment_required WITH a restricted
// 'must_enrol' session cookie (Req 33.7).
func TestLoginEnrolmentRequiredReturns202(t *testing.T) {
	e := newTestEnv(t)
	setLoginablePassword(t, e, "alice", "hunter2xY!")

	// Require 2FA for tenant 1 (alice's tenant); alice is not enrolled.
	if _, err := e.central.Exec(
		`INSERT INTO "TWO_FACTOR_POLICY"(tenant_id, required) VALUES(1, 1)`,
	); err != nil {
		t.Fatalf("set tenant policy required: %v", err)
	}

	rec := e.do(t, http.MethodPost, "/api/auth/login", "", loginRequest{Username: "alice", Password: "hunter2xY!"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	if got := decodeStatus(t, rec.Body.Bytes()); got != "enrolment_required" {
		t.Fatalf("status field = %q, want enrolment_required", got)
	}
	if !hasSessionCookie(rec.Result()) {
		t.Fatal("enrolment-required must set the restricted must_enrol session cookie")
	}
}

// TestLoginRecoveryCodeReturns200 verifies a valid Recovery_Code establishes a
// 200 'active' session and is consumed so it cannot be reused (Req 33.6).
func TestLoginRecoveryCodeReturns200(t *testing.T) {
	e := newTestEnv(t)
	setLoginablePassword(t, e, "alice", "hunter2xY!")
	enrolViaSecret(t, e, "alice")

	const recovery = "BACKUPCODE7654321"
	hash, err := service.HashPassword(recovery)
	if err != nil {
		t.Fatalf("hash recovery code: %v", err)
	}
	uid := userIDByName(t, e, "alice")
	if _, err := e.central.Exec(
		`INSERT INTO "RECOVERY_CODE"(user_id, code_hash) VALUES(?, ?)`, uid, hash,
	); err != nil {
		t.Fatalf("insert recovery code: %v", err)
	}

	rec := e.do(t, http.MethodPost, "/api/auth/login", "",
		loginRequest{Username: "alice", Password: "hunter2xY!", Code: recovery})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !hasSessionCookie(rec.Result()) {
		t.Fatal("a valid recovery code must establish a session cookie")
	}

	// Reuse is rejected with 401.
	rec2 := e.do(t, http.MethodPost, "/api/auth/login", "",
		loginRequest{Username: "alice", Password: "hunter2xY!", Code: recovery})
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("reused recovery-code status = %d, want 401 (body %q)", rec2.Code, rec2.Body.String())
	}
}
