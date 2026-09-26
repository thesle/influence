package service

// Unit tests for the enrolment half of Two-Factor Authentication (Req 33.1,
// 33.2, 33.3). They exercise TwoFactorService directly over a real in-memory
// Central_Directory (newTestConns/seedTenant/seedUser from auth_test.go), so
// the pending-secret storage, the real library TOTP validation, and the hashed
// Recovery_Code issuance are proven at the service boundary — no fakes, and the
// TOTP is computed from the stored secret via the same library the service
// verifies with.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/pquerna/otp/totp"
)

// twoFactorEnrolled reads the caller's two_factor_enrolled flag straight from
// the Central USER row, so a test can assert enrolment state independent of the
// service.
func twoFactorEnrolled(t *testing.T, svc *TwoFactorService, userID int64) bool {
	t.Helper()
	var enrolled int
	if err := svc.central.QueryRow(
		`SELECT two_factor_enrolled FROM "USER" WHERE id = ?`, userID,
	).Scan(&enrolled); err != nil {
		t.Fatalf("read enrolled flag: %v", err)
	}
	return enrolled != 0
}

// pendingSecretOf reads the stored (pending or active) TOTP_Secret for a user.
func pendingSecretOf(t *testing.T, svc *TwoFactorService, userID int64) string {
	t.Helper()
	var secret string
	if err := svc.central.QueryRow(
		`SELECT COALESCE(two_factor_secret, '') FROM "USER" WHERE id = ?`, userID,
	).Scan(&secret); err != nil {
		t.Fatalf("read pending secret: %v", err)
	}
	return secret
}

// TestBeginEnrolmentStoresPendingSecretAndReturnsURI verifies BeginEnrolment
// mints a TOTP_Secret, stores it as pending (secret set, enrolled still 0), and
// returns both the base32 secret and a well-formed otpauth:// URI carrying that
// secret (Req 33.1).
func TestBeginEnrolmentStoresPendingSecretAndReturnsURI(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	svc := NewTwoFactorService(conns)

	secret, uri, err := svc.BeginEnrolment(context.Background(), userIDFromInt(userID))
	if err != nil {
		t.Fatalf("begin enrolment: %v", err)
	}
	if secret == "" {
		t.Fatal("secret should not be empty")
	}
	// The secret is stored as pending, but the user is not yet enrolled.
	if got := pendingSecretOf(t, svc, userID); got != secret {
		t.Fatalf("stored pending secret = %q, want returned secret %q", got, secret)
	}
	if twoFactorEnrolled(t, svc, userID) {
		t.Fatal("user should NOT be enrolled after begin — the secret is only pending")
	}
	// The provisioning URI is an otpauth:// TOTP URL that embeds the secret and
	// the fixed issuer, so an authenticator app can scan it.
	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("uri = %q, want an otpauth://totp/ provisioning URI", uri)
	}
	if !strings.Contains(uri, "secret="+secret) {
		t.Fatalf("uri %q should carry secret=%s", uri, secret)
	}
	if !strings.Contains(uri, "issuer="+twoFactorIssuer) {
		t.Fatalf("uri %q should carry issuer=%s", uri, twoFactorIssuer)
	}
}

// TestConfirmEnrolmentValidCodeEnrollsAndIssuesRecoveryCodes verifies that a
// TOTP computed from the pending secret confirms enrolment: the user is marked
// enrolled and a set of single-use Recovery_Codes is returned, each stored only
// as an Argon2id hash that verifies against the plaintext (Req 33.2).
func TestConfirmEnrolmentValidCodeEnrollsAndIssuesRecoveryCodes(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	svc := NewTwoFactorService(conns)

	secret, _, err := svc.BeginEnrolment(context.Background(), userIDFromInt(userID))
	if err != nil {
		t.Fatalf("begin enrolment: %v", err)
	}

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp: %v", err)
	}

	codes, err := svc.ConfirmEnrolment(context.Background(), userIDFromInt(userID), code)
	if err != nil {
		t.Fatalf("confirm enrolment: %v", err)
	}
	if len(codes) != recoveryCodeCount {
		t.Fatalf("recovery codes = %d, want %d", len(codes), recoveryCodeCount)
	}
	if !twoFactorEnrolled(t, svc, userID) {
		t.Fatal("user should be enrolled after a valid confirm")
	}

	// Every plaintext code must be present in RECOVERY_CODE as a verifying
	// Argon2id hash — and the plaintext itself must NOT be stored.
	assertRecoveryCodesStoredHashed(t, svc, userID, codes)
}

// TestConfirmEnrolmentInvalidCodeStaysPending verifies that an invalid TOTP
// leaves enrolment pending: the user is not enrolled, no Recovery_Codes are
// issued, the pending secret is untouched, and ErrTwoFactorInvalidCode is
// returned (Req 33.3).
func TestConfirmEnrolmentInvalidCodeStaysPending(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	svc := NewTwoFactorService(conns)

	secret, _, err := svc.BeginEnrolment(context.Background(), userIDFromInt(userID))
	if err != nil {
		t.Fatalf("begin enrolment: %v", err)
	}

	// "000000" is overwhelmingly unlikely to be the valid code for the current
	// step; if it happened to be, the compute-and-avoid below would catch it.
	bad := "000000"
	if valid, _ := totp.GenerateCode(secret, time.Now()); valid == bad {
		bad = "000001"
	}

	codes, err := svc.ConfirmEnrolment(context.Background(), userIDFromInt(userID), bad)
	if err == nil {
		t.Fatal("confirm with an invalid code should fail")
	}
	if !errors.Is(err, ErrTwoFactorInvalidCode) {
		t.Fatalf("error = %v, want ErrTwoFactorInvalidCode", err)
	}
	if codes != nil {
		t.Fatalf("recovery codes = %v, want nil on invalid confirm", codes)
	}
	if twoFactorEnrolled(t, svc, userID) {
		t.Fatal("user should remain unenrolled after an invalid confirm")
	}
	// The pending secret is untouched so the user can retry.
	if got := pendingSecretOf(t, svc, userID); got != secret {
		t.Fatalf("pending secret changed after invalid confirm: got %q want %q", got, secret)
	}
	// No Recovery_Codes were written.
	if n := countRecoveryCodes(t, svc, userID); n != 0 {
		t.Fatalf("recovery code rows = %d, want 0 after an invalid confirm", n)
	}
}

// TestConfirmEnrolmentWithoutPendingSecret verifies confirm before begin is
// rejected with ErrTwoFactorNoPendingSecret rather than treated as a wrong
// code.
func TestConfirmEnrolmentWithoutPendingSecret(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	svc := NewTwoFactorService(conns)

	_, err := svc.ConfirmEnrolment(context.Background(), userIDFromInt(userID), "123456")
	if err == nil {
		t.Fatal("confirm with no pending secret should fail")
	}
	if !errors.Is(err, ErrTwoFactorNoPendingSecret) {
		t.Fatalf("error = %v, want ErrTwoFactorNoPendingSecret", err)
	}
}

// TestRecoveryCodesAreDistinctAndHashed verifies the issued codes are unique and
// that each stored hash is a PHC-style Argon2id string, not the plaintext.
func TestRecoveryCodesAreDistinctAndHashed(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")
	svc := NewTwoFactorService(conns)

	secret, _, err := svc.BeginEnrolment(context.Background(), userIDFromInt(userID))
	if err != nil {
		t.Fatalf("begin enrolment: %v", err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp: %v", err)
	}
	codes, err := svc.ConfirmEnrolment(context.Background(), userIDFromInt(userID), code)
	if err != nil {
		t.Fatalf("confirm enrolment: %v", err)
	}

	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if c == "" {
			t.Fatal("recovery code should not be empty")
		}
		if seen[c] {
			t.Fatalf("duplicate recovery code %q", c)
		}
		seen[c] = true
	}

	// The stored hashes must be Argon2id PHC strings, never the plaintext.
	rows, err := svc.central.Query(`SELECT code_hash FROM "RECOVERY_CODE" WHERE user_id = ?`, userID)
	if err != nil {
		t.Fatalf("query recovery codes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatalf("scan hash: %v", err)
		}
		if !strings.HasPrefix(h, "$argon2id$") {
			t.Fatalf("stored code_hash %q is not an Argon2id hash", h)
		}
		if seen[h] {
			t.Fatalf("stored code_hash equals a plaintext code %q — codes must be stored hashed", h)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
}

// --- small local helpers -----------------------------------------------------

// userIDFromInt converts a seeded int64 user id into the data.UserID the
// service expects.
func userIDFromInt(id int64) data.UserID { return data.UserID(id) }

// countRecoveryCodes returns the number of RECOVERY_CODE rows for a user.
func countRecoveryCodes(t *testing.T, svc *TwoFactorService, uid int64) int {
	t.Helper()
	var n int
	if err := svc.central.QueryRow(
		`SELECT COUNT(*) FROM "RECOVERY_CODE" WHERE user_id = ?`, uid,
	).Scan(&n); err != nil {
		t.Fatalf("count recovery codes: %v", err)
	}
	return n
}

// assertRecoveryCodesStoredHashed confirms each returned plaintext code matches
// exactly one stored Argon2id hash (via VerifyPassword) and that the count of
// stored rows equals the number of issued codes.
func assertRecoveryCodesStoredHashed(t *testing.T, svc *TwoFactorService, uid int64, plaintext []string) {
	t.Helper()
	if n := countRecoveryCodes(t, svc, uid); n != len(plaintext) {
		t.Fatalf("stored recovery code rows = %d, want %d", n, len(plaintext))
	}
	rows, err := svc.central.Query(`SELECT code_hash FROM "RECOVERY_CODE" WHERE user_id = ?`, uid)
	if err != nil {
		t.Fatalf("query recovery codes: %v", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatalf("scan hash: %v", err)
		}
		hashes = append(hashes, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	for _, code := range plaintext {
		matched := false
		for _, h := range hashes {
			ok, verr := VerifyPassword(code, h)
			if verr != nil {
				t.Fatalf("verify recovery code hash: %v", verr)
			}
			if ok {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("plaintext recovery code %q does not verify against any stored hash", code)
		}
	}
}
