package api

// Integration tests for the Two-Factor Authentication enrolment endpoints
// (Req 33.1, 33.2, 33.3), driven end to end through Server.Router() and the
// real middleware chain over a real Central_Directory (newTestEnv). They prove
// the routes are mounted inside the authenticated (non-admin) group, delegate
// to TwoFactorService, and shape success/error responses with the standard
// envelope — including a real TOTP computed from the returned secret.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// enrol drives POST /api/2fa/enrol for the given session and returns the decoded
// secret + provisioning URI response.
func enrol(t *testing.T, e *testEnv, token string) twoFactorEnrolResponse {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/2fa/enrol", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("enrol status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body twoFactorEnrolResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode enrol response %q: %v", rec.Body.String(), err)
	}
	return body
}

// TestTwoFactorEnrolRequiresSession verifies both 2FA routes sit behind the
// chain: with no session cookie they are rejected with 401 before any handler
// runs (Req 3.5).
func TestTwoFactorEnrolRequiresSession(t *testing.T) {
	e := newTestEnv(t)
	for _, target := range []string{"/api/2fa/enrol", "/api/2fa/confirm"} {
		rec := e.do(t, http.MethodPost, target, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401 (body %q)", target, rec.Code, rec.Body.String())
		}
		if code := decodeEnvelopeCode(t, rec); code != codeUnauthenticated {
			t.Errorf("%s error code = %q, want %q", target, code, codeUnauthenticated)
		}
	}
}

// TestTwoFactorEnrolReturnsSecretAndURI verifies enrol returns a base32 secret
// and an otpauth:// provisioning URI, and stores the secret as pending without
// yet enrolling the user (Req 33.1).
func TestTwoFactorEnrolReturnsSecretAndURI(t *testing.T) {
	e := newTestEnv(t)
	body := enrol(t, e, e.sessionToken)

	if body.Secret == "" {
		t.Fatal("secret should not be empty")
	}
	if !strings.HasPrefix(body.URI, "otpauth://totp/") {
		t.Fatalf("uri = %q, want an otpauth://totp/ provisioning URI", body.URI)
	}

	// The secret is stored as pending, but enrolment has not completed.
	var secret string
	var enrolled int
	if err := e.central.QueryRow(
		`SELECT COALESCE(two_factor_secret, ''), two_factor_enrolled FROM "USER" WHERE username = 'alice'`,
	).Scan(&secret, &enrolled); err != nil {
		t.Fatalf("read user 2fa state: %v", err)
	}
	if secret != body.Secret {
		t.Fatalf("stored pending secret = %q, want %q", secret, body.Secret)
	}
	if enrolled != 0 {
		t.Fatal("user should not be enrolled after enrol — the secret is only pending")
	}
}

// TestTwoFactorConfirmValidCodeEnrolls verifies a TOTP computed from the pending
// secret confirms enrolment: 200 with a non-empty set of plaintext recovery
// codes, the USER marked enrolled, and the codes stored hashed (Req 33.2).
func TestTwoFactorConfirmValidCodeEnrolls(t *testing.T) {
	e := newTestEnv(t)
	body := enrol(t, e, e.sessionToken)

	code, err := totp.GenerateCode(body.Secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp: %v", err)
	}

	rec := e.do(t, http.MethodPost, "/api/2fa/confirm", e.sessionToken, twoFactorConfirmRequest{Code: code})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var confirm twoFactorConfirmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &confirm); err != nil {
		t.Fatalf("decode confirm response %q: %v", rec.Body.String(), err)
	}
	if len(confirm.RecoveryCodes) == 0 {
		t.Fatal("confirm should return single-use recovery codes")
	}

	var enrolled int
	if err := e.central.QueryRow(
		`SELECT two_factor_enrolled FROM "USER" WHERE username = 'alice'`,
	).Scan(&enrolled); err != nil {
		t.Fatalf("read enrolled flag: %v", err)
	}
	if enrolled != 1 {
		t.Fatal("user should be enrolled after a valid confirm")
	}

	// Recovery codes are stored hashed, not in plaintext.
	var rowCount int
	if err := e.central.QueryRow(
		`SELECT COUNT(*) FROM "RECOVERY_CODE" WHERE code_hash LIKE '$argon2id$%'`,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count hashed recovery codes: %v", err)
	}
	if rowCount != len(confirm.RecoveryCodes) {
		t.Fatalf("hashed recovery code rows = %d, want %d", rowCount, len(confirm.RecoveryCodes))
	}
}

// TestTwoFactorConfirmInvalidCodeStaysPending verifies an invalid TOTP is a 400
// VALIDATION and leaves the user unenrolled with the pending secret intact
// (Req 33.3).
func TestTwoFactorConfirmInvalidCodeStaysPending(t *testing.T) {
	e := newTestEnv(t)
	body := enrol(t, e, e.sessionToken)

	// Choose a code that is definitely not the current valid one.
	bad := "000000"
	if valid, _ := totp.GenerateCode(body.Secret, time.Now()); valid == bad {
		bad = "000001"
	}

	rec := e.do(t, http.MethodPost, "/api/2fa/confirm", e.sessionToken, twoFactorConfirmRequest{Code: bad})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}

	var enrolled int
	var pending string
	if err := e.central.QueryRow(
		`SELECT two_factor_enrolled, COALESCE(two_factor_secret, '') FROM "USER" WHERE username = 'alice'`,
	).Scan(&enrolled, &pending); err != nil {
		t.Fatalf("read user 2fa state: %v", err)
	}
	if enrolled != 0 {
		t.Fatal("user must remain unenrolled after an invalid confirm")
	}
	if pending != body.Secret {
		t.Fatalf("pending secret changed after invalid confirm: got %q want %q", pending, body.Secret)
	}

	var rowCount int
	if err := e.central.QueryRow(`SELECT COUNT(*) FROM "RECOVERY_CODE"`).Scan(&rowCount); err != nil {
		t.Fatalf("count recovery codes: %v", err)
	}
	if rowCount != 0 {
		t.Fatalf("recovery code rows = %d, want 0 after an invalid confirm", rowCount)
	}
}

// TestTwoFactorConfirmBeforeEnrol verifies confirm called before enrol (no
// pending secret) is a 400 VALIDATION rather than a server error.
func TestTwoFactorConfirmBeforeEnrol(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodPost, "/api/2fa/confirm", e.sessionToken, twoFactorConfirmRequest{Code: "123456"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
}
