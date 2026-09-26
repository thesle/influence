package service

// Two-Factor Authentication enrolment (design § "Two-Factor Authentication —
// Req 33", Requirements 33.1, 33.2, 33.3). The TwoFactorService owns the
// enrolment half of 2FA: minting a TOTP_Secret and provisioning URI (Begin),
// and confirming a submitted TOTP against that pending secret before marking
// the user enrolled and issuing single-use Recovery_Codes (Confirm).
//
// TOTP is RFC 6238 — SHA-1, 6 digits, a 30-second step — implemented with the
// well-established github.com/pquerna/otp library rather than a hand-rolled
// HMAC loop. The library's defaults (SHA-1 / 6 digits / 30s) match the design,
// and totp.Validate tolerates one step of clock drift on either side of the
// current step, which is exactly the acceptance window Req 33.9 calls for.
//
// The QR is NOT rendered server-side: BeginEnrolment returns the base32 secret
// and the otpauth:// provisioning URI, and the web application draws the QR
// from that URI client-side (design.md — "no image bytes stored server-side").
//
// All identity + 2FA state lives in the Central_Directory (Req 1.1), so this
// service reads the shared Central handle and never touches a tenant database,
// mirroring IdentityService and AuthService. Every lookup and mutation is keyed
// by the caller's own user id from the RequestContext, so it can only ever
// enrol the caller themselves.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"

	"github.com/pquerna/otp/totp"

	"github.com/influence/influence/internal/data"
)

// twoFactorIssuer is the issuer shown in an authenticator app and encoded in
// the otpauth:// provisioning URI. A fixed product name keeps every account in
// an app grouped under one issuer regardless of tenant (design.md — "a fixed
// issuer \"Influence\" plus the username").
const twoFactorIssuer = "Influence"

// recoveryCodeCount is the number of single-use Recovery_Codes issued when a
// user completes enrolment (Req 33.2). Ten codes is a common, ergonomic backup
// set — enough to print once and use sparingly when the authenticator device is
// unavailable.
const recoveryCodeCount = 10

// recoveryCodeBytes is the number of random bytes behind each Recovery_Code
// before base32 encoding. Ten bytes yields 80 bits of entropy — far beyond
// guessable — and encodes to a 16-character code.
const recoveryCodeBytes = 10

// ErrTwoFactorInvalidCode is returned by ConfirmEnrolment when the submitted
// TOTP does not match the pending secret (Req 33.3). It is a VALIDATION-class
// error: the REST layer maps it to a 400 VALIDATION envelope, and enrolment is
// left pending — the user may retry with a fresh code.
var ErrTwoFactorInvalidCode = errors.New("invalid two-factor code")

// ErrTwoFactorNoPendingSecret is returned by ConfirmEnrolment when the user has
// no pending TOTP_Secret to confirm against — i.e. confirm was called before
// begin. It signals a client-sequencing error rather than a wrong code.
var ErrTwoFactorNoPendingSecret = errors.New("no pending two-factor enrolment")

// TwoFactorService implements the enrolment half of Two-Factor Authentication
// over the shared Central_Directory. It holds only the Central handle; the
// caller's user id (from the RequestContext) scopes every operation, so it can
// only enrol the caller themselves.
type TwoFactorService struct {
	central *sql.DB
}

// NewTwoFactorService constructs a TwoFactorService over the shared
// Central_Directory handle obtained from the connection manager. 2FA state is
// platform-wide (Req 1.1), so it lives in Central, not any tenant database.
func NewTwoFactorService(conns data.ConnManager) *TwoFactorService {
	return &TwoFactorService{central: conns.Central()}
}

// BeginEnrolment starts Two-Factor Authentication enrolment for the caller
// (Req 33.1). It generates a fresh random TOTP_Secret, stores it as *pending*
// in USER.two_factor_secret (the secret is written but two_factor_enrolled
// stays 0, so it is not yet enforced), and returns the base32 secret plus the
// otpauth:// provisioning URI. The web application renders the URI as a QR and
// shows the secret in text form.
//
// Calling Begin again before confirming simply replaces the pending secret with
// a new one — the user re-scans and the previous pending secret is discarded.
func (s *TwoFactorService) BeginEnrolment(ctx context.Context, userID data.UserID) (secret string, uri string, err error) {
	username, err := s.loadUsername(ctx, userID)
	if err != nil {
		return "", "", err
	}

	// The library defaults are exactly the design's parameters: SHA-1, 6
	// digits, 30-second period. It generates a random secret of the default
	// size and builds the otpauth:// URL for us.
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      twoFactorIssuer,
		AccountName: username,
	})
	if err != nil {
		return "", "", fmt.Errorf("generate totp key: %w", err)
	}

	secret = key.Secret()

	// Store the secret as pending. two_factor_enrolled is left untouched (it is
	// still 0 for an unenrolled user), so the secret exists but 2FA is not yet
	// enforced until ConfirmEnrolment succeeds (Req 33.1).
	res, err := s.central.ExecContext(ctx,
		`UPDATE "USER" SET two_factor_secret = ? WHERE id = ?`,
		secret, int64(userID),
	)
	if err != nil {
		return "", "", fmt.Errorf("store pending two-factor secret: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", "", fmt.Errorf("begin two-factor enrolment: no USER row for id %d", int64(userID))
	}

	return secret, key.URL(), nil
}

// ConfirmEnrolment verifies a submitted TOTP against the caller's pending
// TOTP_Secret and, on success, completes enrolment (Req 33.2, 33.3). A valid
// code marks two_factor_enrolled = 1 and issues a set of single-use
// Recovery_Codes: each is returned in plaintext ONCE and stored only as an
// Argon2id hash in RECOVERY_CODE. The plaintext codes are the caller's sole
// copy — they are not recoverable afterwards.
//
// An invalid code leaves enrolment pending (two_factor_enrolled stays 0, the
// pending secret is untouched) and returns ErrTwoFactorInvalidCode, so the user
// may retry (Req 33.3). Confirm called with no pending secret returns
// ErrTwoFactorNoPendingSecret.
//
// The success path runs in a single Central transaction so that flipping the
// enrolled flag and inserting the Recovery_Codes commit together: a partial
// enrolment (enrolled with no backup codes, or codes with the flag still off)
// is never observable.
func (s *TwoFactorService) ConfirmEnrolment(ctx context.Context, userID data.UserID, code string) (recoveryCodes []string, err error) {
	pending, err := s.loadPendingSecret(ctx, userID)
	if err != nil {
		return nil, err
	}

	// Validate against the pending secret. totp.Validate uses the library
	// defaults (SHA-1, 6 digits, 30s) and accepts the current step plus one
	// step of drift on each side, matching the acceptance window of Req 33.9.
	if !totp.Validate(code, pending) {
		// Invalid code: enrolment stays pending, nothing is mutated (Req 33.3).
		return nil, ErrTwoFactorInvalidCode
	}

	// Generate the plaintext codes and their Argon2id hashes up front so a
	// hashing failure aborts before any write.
	plain := make([]string, recoveryCodeCount)
	hashes := make([]string, recoveryCodeCount)
	for i := range plain {
		c, err := newRecoveryCode()
		if err != nil {
			return nil, fmt.Errorf("generate recovery code: %w", err)
		}
		h, err := HashPassword(c)
		if err != nil {
			return nil, fmt.Errorf("hash recovery code: %w", err)
		}
		plain[i] = c
		hashes[i] = h
	}

	tx, err := s.central.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin confirm transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE "USER" SET two_factor_enrolled = 1 WHERE id = ?`,
		int64(userID),
	); err != nil {
		return nil, fmt.Errorf("mark two-factor enrolled: %w", err)
	}

	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "RECOVERY_CODE" (user_id, code_hash) VALUES (?, ?)`,
			int64(userID), h,
		); err != nil {
			return nil, fmt.Errorf("store recovery code: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit confirm transaction: %w", err)
	}

	return plain, nil
}

// SetTenantPolicy upserts the caller's tenant Two_Factor_Policy to
// optional/required (Req 33.7, 33.8). The tenant is taken from the
// RequestContext, never from client input, so an admin can only ever toggle
// their own tenant's policy — the (tenant_id) primary key on TWO_FACTOR_POLICY
// means at most one row per tenant, and the upsert flips its required flag in
// place. An absent row previously meant optional; writing required=0 is
// equivalent and keeps the row present for a later toggle.
//
// The required flag is stored as the integer 0/1 the column's CHECK constraint
// permits. This is an admin operation; the route guard (RequireAdmin) enforces
// the admin check, so the service trusts the caller's tenant from the context.
func (s *TwoFactorService) SetTenantPolicy(ctx context.Context, rc data.RequestContext, required bool) error {
	req := 0
	if required {
		req = 1
	}
	if _, err := s.central.ExecContext(ctx,
		`INSERT INTO "TWO_FACTOR_POLICY" (tenant_id, required) VALUES (?, ?)
		 ON CONFLICT(tenant_id) DO UPDATE SET required = excluded.required`,
		int64(rc.TenantID()), req,
	); err != nil {
		return fmt.Errorf("set tenant two-factor policy: %w", err)
	}
	return nil
}

// loadUsername reads the caller's username from the Central_Directory USER row,
// used as the account name in the otpauth:// provisioning URI. A missing row is
// an inconsistency (the caller reached here through a validated session), so it
// is surfaced as an error rather than a silent default.
func (s *TwoFactorService) loadUsername(ctx context.Context, userID data.UserID) (string, error) {
	var username string
	err := s.central.QueryRowContext(ctx,
		`SELECT username FROM "USER" WHERE id = ?`,
		int64(userID),
	).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("begin two-factor enrolment: no USER row for id %d", int64(userID))
	}
	if err != nil {
		return "", fmt.Errorf("load username: %w", err)
	}
	return username, nil
}

// loadPendingSecret reads the caller's pending TOTP_Secret. A NULL secret means
// enrolment was never begun, so it returns ErrTwoFactorNoPendingSecret; a
// missing USER row is an inconsistency surfaced as an error.
func (s *TwoFactorService) loadPendingSecret(ctx context.Context, userID data.UserID) (string, error) {
	var secret sql.NullString
	err := s.central.QueryRowContext(ctx,
		`SELECT two_factor_secret FROM "USER" WHERE id = ?`,
		int64(userID),
	).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("confirm two-factor enrolment: no USER row for id %d", int64(userID))
	}
	if err != nil {
		return "", fmt.Errorf("load pending two-factor secret: %w", err)
	}
	if !secret.Valid || secret.String == "" {
		return "", ErrTwoFactorNoPendingSecret
	}
	return secret.String, nil
}

// newRecoveryCode returns a single random Recovery_Code: recoveryCodeBytes of
// cryptographically random data, base32-encoded without padding to a short,
// human-transcribable string. The plaintext is shown to the user once; only its
// Argon2id hash is stored.
func newRecoveryCode() (string, error) {
	buf := make([]byte, recoveryCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}
