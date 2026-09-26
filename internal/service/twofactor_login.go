// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package service

// Second-factor integration for the login flow (Req 33.4, 33.5, 33.6, 33.9;
// design.md — "Login integration (33.4–33.6, 33.9)").
//
// LoginWithFactor is the login entry point the REST layer uses once 2FA exists.
// It performs the same password/lockout/deactivation/policy steps as the bare
// Login (auth.go) and then, for a user who has Two_Factor_Authentication
// enrolled, requires a valid second factor before it will establish an 'active'
// session:
//
//   - A TOTP from the current 30-second step or either adjacent step (±1) is
//     accepted to tolerate clock drift (Req 33.9). The consumed step is
//     recorded in USER.last_totp_step; a TOTP whose matched step is not strictly
//     greater than last_totp_step is rejected as a replay within its window
//     (Req 33.9). This is the anti-replay the design's per-user last_totp_step
//     guards.
//   - Otherwise a single-use Recovery_Code is accepted: the submitted code is
//     verified against each unused RECOVERY_CODE.code_hash with the same
//     Argon2id VerifyPassword used for passwords, and on a match that row's
//     used flag flips to 1 so it can never be redeemed again (Req 33.6).
//
// A wrong second factor (neither a valid TOTP nor an unused Recovery_Code) is an
// authentication failure that establishes no session and feeds the SAME
// failed_attempts/locked_until lockout password failures use — repeated
// second-factor failures lock the account exactly as repeated wrong passwords
// do (Req 33.5, reusing Req 3.3).
//
// The three login outcomes the handler distinguishes:
//
//   - not enrolled, policy not required          → 'active' session   (created)
//   - not enrolled, tenant policy required        → 'must_enrol' session (created)
//   - enrolled, no code supplied                  → ErrSecondFactorRequired (no session)
//   - enrolled, valid TOTP / Recovery_Code        → 'active' session   (created)
//   - enrolled, invalid second factor             → ErrInvalidCredentials (+lockout)
//
// The bare Login in auth.go is left unchanged as the password/policy step; the
// TOTP step math and recovery-code redemption live here so auth.go stays lean.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// ErrSecondFactorRequired is returned by LoginWithFactor when the password is
// correct and the user has Two_Factor_Authentication enrolled, but no second
// factor was supplied. It establishes no session: the caller must re-submit the
// username, password, and a TOTP or Recovery_Code. The REST layer maps this to
// a 202 "second factor required" response distinct from the 200 that a fully
// authenticated login returns.
var ErrSecondFactorRequired = errors.New("second factor required")

// totpStepSeconds is the TOTP time-step in seconds — RFC 6238's default and the
// library default the enrolment path uses. The accepted drift window is this
// step and the two adjacent steps (Req 33.9).
const totpStepSeconds = 30

// LoginWithFactor verifies a username/password and, for an enrolled user, a
// second factor (TOTP or Recovery_Code) before establishing an 'active' session
// (Req 33.4, 33.5, 33.6, 33.9). See the file comment for the full outcome table.
//
// It shares the password, lockout, deactivation, and tenant-policy semantics of
// the bare Login: an unknown user or wrong password is ErrInvalidCredentials, a
// deactivated account is ErrAccountDeactivated, and an active lock is
// ErrAccountLocked — all establishing no session.
func (s *AuthService) LoginWithFactor(ctx context.Context, username, password, code string) (Session, error) {
	now := s.clock()

	var (
		userID       int64
		tenantID     int64
		passwordHash string
		lockedUntil  sql.NullString
		deactivated  int
		enrolled     int
		secret       sql.NullString
		lastStep     sql.NullInt64
	)
	err := s.central.QueryRowContext(ctx,
		`SELECT id, tenant_id, password_hash, locked_until, deactivated,
		        two_factor_enrolled, two_factor_secret, last_totp_step
		   FROM "USER" WHERE username = ?`,
		username,
	).Scan(&userID, &tenantID, &passwordHash, &lockedUntil, &deactivated,
		&enrolled, &secret, &lastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, fmt.Errorf("lookup user: %w", err)
	}

	if deactivated != 0 {
		return Session{}, ErrAccountDeactivated
	}

	if locked, err := lockActive(lockedUntil, now); err != nil {
		return Session{}, err
	} else if locked {
		return Session{}, ErrAccountLocked
	}

	ok, err := VerifyPassword(password, passwordHash)
	if err != nil {
		return Session{}, ErrInvalidCredentials
	}
	if !ok {
		// Wrong password: feed the shared lockout and deny (Req 3.3, 3.2).
		if lockErr := s.registerFailedAttempt(ctx, userID, now); lockErr != nil {
			return Session{}, lockErr
		}
		return Session{}, ErrInvalidCredentials
	}

	// Password verified. An unenrolled user follows the existing policy path:
	// 'must_enrol' when the tenant requires 2FA, else 'active' (Req 33.7, 33.8).
	// No second factor is owed because none is enrolled.
	if enrolled == 0 {
		state := SessionStateActive
		required, err := s.tenantPolicyRequired(ctx, tenantID)
		if err != nil {
			return Session{}, err
		}
		if required {
			state = SessionStateMustEnrol
		}
		return s.createSession(ctx, userID, tenantID, now, state)
	}

	// Enrolled: a second factor is required before any session is established
	// (Req 33.4). With no code supplied the caller is told to re-submit with
	// one; no session is created so there is no usable cookie in this state.
	if code == "" {
		return Session{}, ErrSecondFactorRequired
	}

	// Try the code as a TOTP first (the common case), then as a Recovery_Code.
	// A success on either path establishes an 'active' session; a failure on
	// both feeds the shared lockout and denies (Req 33.5).
	verified, err := s.verifySecondFactor(ctx, userID, secret, lastStep, code, now)
	if err != nil {
		return Session{}, err
	}
	if !verified {
		if lockErr := s.registerFailedAttempt(ctx, userID, now); lockErr != nil {
			return Session{}, lockErr
		}
		return Session{}, ErrInvalidCredentials
	}

	return s.createSession(ctx, userID, tenantID, now, SessionStateActive)
}

// verifySecondFactor reports whether the submitted code is a valid second factor
// for the enrolled user: a TOTP within the current step ±1 that has not already
// been consumed, or an unused Recovery_Code. A matched TOTP records its step in
// USER.last_totp_step (anti-replay, Req 33.9); a matched Recovery_Code flips its
// used flag to 1 (single use, Req 33.6). It returns (false, nil) when the code
// matches neither — the caller then applies the lockout and denies.
func (s *AuthService) verifySecondFactor(ctx context.Context, userID int64, secret sql.NullString, lastStep sql.NullInt64, code string, now time.Time) (bool, error) {
	// TOTP path. Only attempted when a secret is present (an enrolled user
	// always has one, but guard defensively).
	if secret.Valid && secret.String != "" {
		matched, step := validateTOTPStep(code, secret.String, now)
		if matched {
			// Reject a replay: the matched step must be strictly newer than the
			// last consumed step. A code re-presented within its (multi-step)
			// window resolves to a step already recorded, so it is refused
			// (Req 33.9).
			if lastStep.Valid && step <= lastStep.Int64 {
				return false, nil
			}
			if _, err := s.central.ExecContext(ctx,
				`UPDATE "USER" SET last_totp_step = ? WHERE id = ?`,
				step, userID,
			); err != nil {
				return false, fmt.Errorf("record consumed totp step: %w", err)
			}
			return true, nil
		}
	}

	// Recovery_Code path: verify the submitted code against each unused code's
	// Argon2id hash, and on a match mark that single row used so it cannot be
	// redeemed again (Req 33.6).
	return s.redeemRecoveryCode(ctx, userID, code)
}

// validateTOTPStep checks a submitted TOTP against the secret for the current
// time step and the two adjacent steps (±1) to tolerate clock drift (Req 33.9).
// On a match it returns the absolute time-step number that matched so the caller
// can record it for replay rejection. Each candidate step is validated with a
// zero skew so the matched step is unambiguous; the ±1 tolerance is expressed by
// probing the neighbouring steps explicitly.
//
// A code that is not even TOTP-shaped (wrong length, non-numeric) makes
// ValidateCustom return an error; that simply means "not a valid TOTP" — a
// Recovery_Code, say — so it is treated as a non-match rather than a failure,
// letting the caller fall through to the recovery-code path.
func validateTOTPStep(code, secret string, now time.Time) (matched bool, step int64) {
	opts := totp.ValidateOpts{
		Period:    totpStepSeconds,
		Skew:      0,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	}
	current := now.Unix() / totpStepSeconds
	// Probe older steps first so the earliest matching step is recorded; this
	// makes the replay comparison against last_totp_step monotonic.
	for _, candidate := range []int64{current - 1, current, current + 1} {
		at := time.Unix(candidate*totpStepSeconds, 0)
		ok, verr := totp.ValidateCustom(code, secret, at, opts)
		if verr != nil {
			// Not a TOTP-shaped code; stop probing steps and defer to the
			// recovery-code path.
			return false, 0
		}
		if ok {
			return true, candidate
		}
	}
	return false, 0
}

// redeemRecoveryCode verifies the submitted code against the user's unused
// Recovery_Codes and, on the first match, marks that row used within a
// transaction so it is consumed exactly once (Req 33.6). It returns true when a
// code was redeemed, false when none matched. Codes are stored only as Argon2id
// hashes, so the plaintext is compared with VerifyPassword — the same primitive
// used for passwords.
func (s *AuthService) redeemRecoveryCode(ctx context.Context, userID int64, code string) (bool, error) {
	if code == "" {
		return false, nil
	}

	tx, err := s.central.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin recovery-code tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, code_hash FROM "RECOVERY_CODE" WHERE user_id = ? AND used = 0`,
		userID,
	)
	if err != nil {
		return false, fmt.Errorf("load recovery codes: %w", err)
	}

	var matchedID int64 = -1
	for rows.Next() {
		var (
			id   int64
			hash string
		)
		if err := rows.Scan(&id, &hash); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan recovery code: %w", err)
		}
		ok, verr := VerifyPassword(code, hash)
		if verr != nil {
			// A corrupt/foreign stored hash is skipped rather than surfaced, so
			// one bad row cannot deny a caller a valid code stored elsewhere.
			continue
		}
		if ok {
			matchedID = id
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("iterate recovery codes: %w", err)
	}
	_ = rows.Close()

	if matchedID < 0 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE "RECOVERY_CODE" SET used = 1 WHERE id = ?`,
		matchedID,
	); err != nil {
		return false, fmt.Errorf("mark recovery code used: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit recovery-code redemption: %w", err)
	}
	return true, nil
}
