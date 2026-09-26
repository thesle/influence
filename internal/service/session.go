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

// Session lifecycle: validation, sliding expiry, logout, and the session
// cookie (Requirements 3.5, 3.6; design.md — "Sessions (3.5, 3.6)").
//
// A session is a server-side SESSION row keyed by an opaque high-entropy token
// (created in auth.go — createSession). This file implements the rest of the
// lifecycle:
//
//   - ValidateSession enforces the two independent bounds that make a session
//     valid: it must have been used within the last 30 minutes
//     (now - last_seen_at <= 30m, the inactivity/idle bound) AND it must be
//     within 24 hours of establishment (now - created_at <= 24h, the absolute
//     bound), whichever is tighter (Req 3.6). On a valid request it slides
//     last_seen_at to now so activity keeps the session alive; an expired or
//     unknown token is rejected and returns no user/tenant.
//   - Logout deletes the SESSION row so the token can no longer authenticate
//     any subsequent request (Req 3.5).
//   - SessionCookie builds the HttpOnly/Secure/SameSite cookie that carries the
//     token to the browser; ClearSessionCookie builds its deletion counterpart
//     for logout.
//
// All timestamp math uses the injected service clock (s.clock) so tests can pin
// "now" deterministically, matching the login/lockout paths.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/influence/influence/internal/data"
)

// ErrSessionInvalid is returned by ValidateSession for any token that does not
// currently authenticate a user: unknown token, or a session that has passed
// either the inactivity or the absolute expiry bound. Collapsing these into one
// error keeps the failure indistinguishable from the caller's perspective — an
// expired session and a forged token both simply fail to authenticate.
var ErrSessionInvalid = errors.New("session invalid or expired")

const (
	// sessionIdleTimeout is the inactivity bound: a session expires after this
	// long without use (Req 3.6 — "30 minutes of inactivity").
	sessionIdleTimeout = 30 * time.Minute
	// sessionAbsoluteLifetime is the absolute bound: a session expires this
	// long after establishment regardless of activity (Req 3.6 — "24 hours
	// after establishment").
	sessionAbsoluteLifetime = 24 * time.Hour
)

// SessionCookieName is the name of the cookie carrying the opaque session
// token. It is exported so the HTTP layer (login/logout handlers, auth
// middleware) can read and write the same cookie.
const SessionCookieName = "influence_session"

// ValidateSession resolves an opaque session token to its user and tenant,
// enforcing the sliding-idle and absolute-lifetime bounds and refreshing the
// session's activity timestamp (Req 3.6).
//
// A session is valid iff BOTH bounds hold against the service clock:
//
//	now - last_seen_at <= 30m   (inactivity / idle)
//	now - created_at   <= 24h   (absolute)
//
// On success the session's last_seen_at is slid forward to now (so continued
// activity keeps the session alive up to the 24h ceiling) and the resolved
// Session is returned. On an unknown token or either bound being exceeded,
// ErrSessionInvalid is returned and no user/tenant is resolved. An expired
// session's row is deleted so it cannot linger; this keeps the SESSION table
// self-pruning as stale tokens are presented.
func (s *AuthService) ValidateSession(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrSessionInvalid
	}

	now := s.clock().UTC()

	var (
		userID       int64
		tenantID     int64
		createdAtStr string
		lastSeenStr  string
		state        string
	)
	// Join to USER to resolve the tenant the session belongs to in a single
	// round-trip; sessions and identity both live in the Central_Directory. The
	// session's state is carried through so the middleware can gate a
	// restricted ('must_enrol') session (Req 33.7).
	err := s.central.QueryRowContext(ctx,
		`SELECT s.user_id, u.tenant_id, s.created_at, s.last_seen_at, s.state
		   FROM "SESSION" s
		   JOIN "USER" u ON u.id = s.user_id
		  WHERE s.token = ?`,
		token,
	).Scan(&userID, &tenantID, &createdAtStr, &lastSeenStr, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionInvalid
	}
	if err != nil {
		return Session{}, fmt.Errorf("lookup session: %w", err)
	}

	createdAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		// A corrupt timestamp fails closed: treat the session as invalid and
		// remove it rather than granting access on unreadable state.
		_ = s.deleteSession(ctx, token)
		return Session{}, ErrSessionInvalid
	}
	lastSeen, err := time.Parse(time.RFC3339Nano, lastSeenStr)
	if err != nil {
		_ = s.deleteSession(ctx, token)
		return Session{}, ErrSessionInvalid
	}

	// Both bounds must hold. Either one being exceeded expires the session
	// (Req 3.6 — "whichever occurs first").
	idleExpired := now.Sub(lastSeen.UTC()) > sessionIdleTimeout
	absoluteExpired := now.Sub(createdAt.UTC()) > sessionAbsoluteLifetime
	if idleExpired || absoluteExpired {
		_ = s.deleteSession(ctx, token)
		return Session{}, ErrSessionInvalid
	}

	// Valid request: slide last_seen_at forward so continued activity keeps the
	// session alive up to the 24h ceiling.
	ts := now.Format(time.RFC3339Nano)
	if _, err := s.central.ExecContext(ctx,
		`UPDATE "SESSION" SET last_seen_at = ? WHERE token = ?`,
		ts, token,
	); err != nil {
		return Session{}, fmt.Errorf("slide session: %w", err)
	}

	return Session{
		Token:     token,
		UserID:    data.UserID(userID),
		TenantID:  data.TenantID(tenantID),
		CreatedAt: createdAt.UTC(),
		State:     state,
	}, nil
}

// Logout terminates a session by deleting its SESSION row, so the token can no
// longer authenticate any subsequent request (Req 3.5). Logging out an unknown
// or already-terminated token is a no-op and returns no error — the desired
// end state (no such session) already holds.
func (s *AuthService) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.deleteSession(ctx, token)
}

// deleteSession removes the SESSION row for a token. Used by Logout and by
// ValidateSession to prune sessions that have expired or carry corrupt
// timestamps.
func (s *AuthService) deleteSession(ctx context.Context, token string) error {
	if _, err := s.central.ExecContext(ctx,
		`DELETE FROM "SESSION" WHERE token = ?`, token,
	); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// SessionCookie builds the cookie that carries an opaque session token to the
// browser (Req 3.6). The cookie is hardened per design.md ("an HttpOnly,
// Secure, SameSite cookie"):
//
//   - HttpOnly: not readable from JavaScript, limiting token theft via XSS.
//   - Secure: only sent over HTTPS, so the token never crosses a plaintext hop.
//   - SameSite=Lax: not sent on cross-site requests, mitigating CSRF while
//     still allowing top-level navigations to the app.
//   - Path "/": the cookie applies to the whole application.
//
// The cookie is deliberately a session cookie (no Max-Age/Expires): the
// authoritative lifetime is the server-side SESSION row and its 30m/24h bounds,
// not a client-controlled expiry. ClearSessionCookie is its logout counterpart.
func SessionCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearSessionCookie builds the cookie that removes the session cookie from the
// browser on logout: an empty value with an immediate expiry (MaxAge < 0). The
// security attributes mirror SessionCookie so the browser matches and replaces
// the existing cookie.
func ClearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}
