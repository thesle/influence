package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/influence/influence/internal/data"
)

// loginAt logs alice in against a freshly seeded Central DB at the given time
// and returns the connection manager, the created session, and the user/tenant
// ids. Sessions and users live in Central, so no tenant DB is needed.
func loginAt(t *testing.T, at time.Time) (data.ConnManager, Session, int64, int64) {
	t.Helper()
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2")

	svc := NewAuthService(conns).withClock(func() time.Time { return at })
	sess, err := svc.Login(context.Background(), "alice", "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return conns, sess, userID, tenantID
}

// svcAt returns an AuthService over conns whose clock is pinned to at.
func svcAt(conns data.ConnManager, at time.Time) *AuthService {
	return NewAuthService(conns).withClock(func() time.Time { return at })
}

// TestValidateSessionFreshRequest asserts a session used shortly after login is
// valid and resolves to the right user and tenant (Req 3.6).
func TestValidateSessionFreshRequest(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, userID, tenantID := loginAt(t, start)

	later := start.Add(5 * time.Minute)
	got, err := svcAt(conns, later).ValidateSession(context.Background(), sess.Token)
	if err != nil {
		t.Fatalf("validate fresh session: %v", err)
	}
	if got.UserID != data.UserID(userID) {
		t.Fatalf("user = %d, want %d", got.UserID, userID)
	}
	if got.TenantID != data.TenantID(tenantID) {
		t.Fatalf("tenant = %d, want %d", got.TenantID, tenantID)
	}
}

// TestValidateSessionSlidesLastSeen asserts a valid request slides last_seen_at
// forward to now, so continued activity keeps the session alive (Req 3.6).
func TestValidateSessionSlidesLastSeen(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, _, _ := loginAt(t, start)
	central := conns.Central()

	req := start.Add(20 * time.Minute)
	if _, err := svcAt(conns, req).ValidateSession(context.Background(), sess.Token); err != nil {
		t.Fatalf("validate: %v", err)
	}

	var lastSeen string
	if err := central.QueryRow(
		`SELECT last_seen_at FROM "SESSION" WHERE token = ?`, sess.Token,
	).Scan(&lastSeen); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	want := req.UTC().Format(time.RFC3339Nano)
	if lastSeen != want {
		t.Fatalf("last_seen_at = %q, want %q", lastSeen, want)
	}
}

// TestValidateSessionSlidingKeepsAlive asserts that repeated activity within the
// idle window keeps the session valid past the raw 30 minutes from login,
// because each request slides last_seen_at (Req 3.6).
func TestValidateSessionSlidingKeepsAlive(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, _, _ := loginAt(t, start)

	// Two requests 25 minutes apart: the second is 50 minutes after login but
	// only 25 minutes after the previous activity, so it stays valid.
	if _, err := svcAt(conns, start.Add(25*time.Minute)).ValidateSession(context.Background(), sess.Token); err != nil {
		t.Fatalf("first slide should be valid: %v", err)
	}
	if _, err := svcAt(conns, start.Add(50*time.Minute)).ValidateSession(context.Background(), sess.Token); err != nil {
		t.Fatalf("second request within idle window should be valid: %v", err)
	}
}

// TestValidateSessionIdleExpiry asserts a session unused for more than 30
// minutes is expired even well within the 24h absolute window (Req 3.6).
func TestValidateSessionIdleExpiry(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, _, _ := loginAt(t, start)

	tooLate := start.Add(30*time.Minute + time.Second)
	_, err := svcAt(conns, tooLate).ValidateSession(context.Background(), sess.Token)
	if !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expected ErrSessionInvalid for idle expiry, got %v", err)
	}

	// The expired session should have been pruned.
	var count int
	if err := conns.Central().QueryRow(
		`SELECT COUNT(*) FROM "SESSION" WHERE token = ?`, sess.Token,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired session should be pruned, found %d rows", count)
	}
}

// TestValidateSessionIdleBoundaryInclusive asserts exactly 30 minutes of
// inactivity is still valid (bound is <=, Req 3.6).
func TestValidateSessionIdleBoundaryInclusive(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, _, _ := loginAt(t, start)

	exactly := start.Add(30 * time.Minute)
	if _, err := svcAt(conns, exactly).ValidateSession(context.Background(), sess.Token); err != nil {
		t.Fatalf("exactly 30m idle should still be valid: %v", err)
	}
}

// TestValidateSessionAbsoluteExpiry asserts a session older than 24 hours is
// expired even when it has been continuously active (Req 3.6). Activity slides
// last_seen_at but never moves created_at, so the absolute ceiling still fires.
func TestValidateSessionAbsoluteExpiry(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, _, _ := loginAt(t, start)

	// Keep the session active every 20 minutes so the idle bound never trips;
	// the absolute 24h bound must still expire it.
	svc := NewAuthService(conns)
	for m := 20; m <= 24*60; m += 20 {
		at := start.Add(time.Duration(m) * time.Minute)
		svc.withClock(func() time.Time { return at })
		if _, err := svc.ValidateSession(context.Background(), sess.Token); err != nil {
			t.Fatalf("kept-alive request at +%dm should be valid: %v", m, err)
		}
	}

	past24h := start.Add(24*time.Hour + time.Second)
	svc.withClock(func() time.Time { return past24h })
	_, err := svc.ValidateSession(context.Background(), sess.Token)
	if !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expected ErrSessionInvalid past 24h absolute lifetime, got %v", err)
	}
}

// TestValidateSessionAbsoluteBoundaryInclusive asserts exactly 24 hours after
// establishment is still valid (bound is <=, Req 3.6).
func TestValidateSessionAbsoluteBoundaryInclusive(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, _, _ := loginAt(t, start)

	// Keep the session active every 20 minutes so the idle bound never trips
	// on the way to the 24h ceiling; then assert exactly 24h is still valid.
	svc := NewAuthService(conns)
	for m := 20; m < 24*60; m += 20 {
		at := start.Add(time.Duration(m) * time.Minute)
		svc.withClock(func() time.Time { return at })
		if _, err := svc.ValidateSession(context.Background(), sess.Token); err != nil {
			t.Fatalf("keep-alive at +%dm: %v", m, err)
		}
	}
	exactly := start.Add(24 * time.Hour)
	svc.withClock(func() time.Time { return exactly })
	if _, err := svc.ValidateSession(context.Background(), sess.Token); err != nil {
		t.Fatalf("exactly 24h should still be valid: %v", err)
	}
}

// TestValidateSessionUnknownToken asserts an unknown or empty token is rejected
// with ErrSessionInvalid and resolves no user.
func TestValidateSessionUnknownToken(t *testing.T) {
	conns := newTestConns(t)
	svc := NewAuthService(conns)

	if _, err := svc.ValidateSession(context.Background(), "nope"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("unknown token: expected ErrSessionInvalid, got %v", err)
	}
	if _, err := svc.ValidateSession(context.Background(), ""); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("empty token: expected ErrSessionInvalid, got %v", err)
	}
}

// TestLogoutTerminatesSession asserts logout deletes the session so subsequent
// validation of the same token fails (Req 3.5).
func TestLogoutTerminatesSession(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, _, _ := loginAt(t, start)
	svc := svcAt(conns, start.Add(time.Minute))

	if err := svc.Logout(context.Background(), sess.Token); err != nil {
		t.Fatalf("logout: %v", err)
	}

	var count int
	if err := conns.Central().QueryRow(
		`SELECT COUNT(*) FROM "SESSION" WHERE token = ?`, sess.Token,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("logout should delete the session row, found %d", count)
	}

	if _, err := svc.ValidateSession(context.Background(), sess.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("post-logout validation: expected ErrSessionInvalid, got %v", err)
	}
}

// TestLogoutUnknownTokenIsNoop asserts logging out a token with no session is a
// harmless no-op (Req 3.5 — desired end state already holds).
func TestLogoutUnknownTokenIsNoop(t *testing.T) {
	conns := newTestConns(t)
	svc := NewAuthService(conns)
	if err := svc.Logout(context.Background(), "ghost"); err != nil {
		t.Fatalf("logout of unknown token should be a no-op, got %v", err)
	}
	if err := svc.Logout(context.Background(), ""); err != nil {
		t.Fatalf("logout of empty token should be a no-op, got %v", err)
	}
}

// TestSessionCookieAttributes asserts the session cookie carries the token and
// the required hardening attributes: HttpOnly, Secure, SameSite (Req 3.6).
func TestSessionCookieAttributes(t *testing.T) {
	c := SessionCookie("tok-123")
	if c.Name != SessionCookieName {
		t.Fatalf("cookie name = %q, want %q", c.Name, SessionCookieName)
	}
	if c.Value != "tok-123" {
		t.Fatalf("cookie value = %q, want %q", c.Value, "tok-123")
	}
	if !c.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Fatal("session cookie must be Secure")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Fatalf("session cookie Path = %q, want %q", c.Path, "/")
	}
}

// TestClearSessionCookieExpires asserts the logout cookie clears the value and
// expires immediately while keeping matching attributes (Req 3.5).
func TestClearSessionCookieExpires(t *testing.T) {
	c := ClearSessionCookie()
	if c.Name != SessionCookieName {
		t.Fatalf("clear cookie name = %q, want %q", c.Name, SessionCookieName)
	}
	if c.Value != "" {
		t.Fatalf("clear cookie value = %q, want empty", c.Value)
	}
	if c.MaxAge >= 0 {
		t.Fatalf("clear cookie MaxAge = %d, want negative (immediate expiry)", c.MaxAge)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatal("clear cookie must keep HttpOnly/Secure/SameSite attributes")
	}
}

// TestValidateSessionCorruptTimestampFailsClosed asserts a session row with an
// unparseable timestamp is treated as invalid and pruned, rather than granting
// access on unreadable state.
func TestValidateSessionCorruptTimestampFailsClosed(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	conns, sess, _, _ := loginAt(t, start)

	if _, err := conns.Central().Exec(
		`UPDATE "SESSION" SET last_seen_at = ? WHERE token = ?`, "not-a-time", sess.Token,
	); err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err := svcAt(conns, start.Add(time.Minute)).ValidateSession(context.Background(), sess.Token)
	if !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("corrupt timestamp should fail closed, got %v", err)
	}
	var count int
	if err := conns.Central().QueryRow(
		`SELECT COUNT(*) FROM "SESSION" WHERE token = ?`, sess.Token,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("corrupt session should be pruned, found %d", count)
	}
}
