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

// Brute-force lockout (Requirement 3.3, task 5.2). This file holds the
// counter-and-lock logic that the login path invokes on a wrong password,
// kept separate from auth.go so the shared Login flow stays minimal.
//
// The rule: each wrong-password login increments USER.failed_attempts; when the
// count of consecutive failures reaches lockoutThreshold, USER.locked_until is
// set to now + lockoutDuration. A successful login resets the counter and clears
// the lock (handled in auth.go's createSession — Req 3.3 reset-on-success).
// Because the counter is only ever cleared on success, the value stored in the
// column is exactly the run of consecutive failures, which is what the
// "5 consecutive times" requirement measures.
//
// Honoring an already-set lock (refusing a login while locked_until is in the
// future) lives in auth.go's Login via lockActive; that path runs before the
// password is even checked, so an attempt during lockout is rejected regardless
// of credential correctness.

import (
	"context"
	"fmt"
	"time"
)

const (
	// lockoutThreshold is the number of consecutive failed login attempts that
	// triggers a lockout. The 5th consecutive failure sets the lock
	// (Requirement 3.3).
	lockoutThreshold = 5

	// lockoutDuration is how long authentication is locked once the threshold
	// is reached (Requirement 3.3 — 15 minutes).
	lockoutDuration = 15 * time.Minute
)

// registerFailedAttempt records one wrong-password login for the given user and
// applies a lockout when the consecutive-failure count reaches
// lockoutThreshold. It increments USER.failed_attempts atomically and, if the
// resulting count is at or above the threshold, sets USER.locked_until to
// now + lockoutDuration (Requirement 3.3).
//
// The increment and the conditional lock run in a single statement pair inside
// one transaction so a concurrent login cannot observe a half-updated state.
// now is supplied by the service clock so the lock window is deterministic in
// tests.
func (s *AuthService) registerFailedAttempt(ctx context.Context, userID int64, now time.Time) error {
	tx, err := s.central.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin lockout tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Increment the consecutive-failure counter and read back the new value.
	var attempts int
	if err := tx.QueryRowContext(ctx,
		`UPDATE "USER" SET failed_attempts = failed_attempts + 1
		  WHERE id = ?
		 RETURNING failed_attempts`,
		userID,
	).Scan(&attempts); err != nil {
		return fmt.Errorf("increment failed_attempts: %w", err)
	}

	// On reaching the threshold, arm the lockout window. Storing the timestamp
	// in the same RFC3339 form auth.go's lockActive parses keeps the two sides
	// consistent.
	if attempts >= lockoutThreshold {
		until := now.Add(lockoutDuration).UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx,
			`UPDATE "USER" SET locked_until = ? WHERE id = ?`,
			until, userID,
		); err != nil {
			return fmt.Errorf("set locked_until: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit lockout tx: %w", err)
	}
	return nil
}
