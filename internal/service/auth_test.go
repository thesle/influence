package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/influence/influence/internal/data"
)

// newTestConns opens a fresh in-memory Central_Directory, migrates it, and
// wraps it in a real ConnManager. Sessions and users live in the Central DB, so
// no tenant database is needed for these auth tests.
func newTestConns(t *testing.T) data.ConnManager {
	t.Helper()
	db, err := sql.Open(data.DriverName, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open in-memory central: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := data.MigrateCentral(context.Background(), db); err != nil {
		t.Fatalf("migrate central: %v", err)
	}
	conns := data.NewConnManager(db)
	t.Cleanup(func() { _ = conns.Close() })
	return conns
}

// seedTenant inserts an active tenant into the Central registry and returns its
// id.
func seedTenant(t *testing.T, central *sql.DB, uuid, name string) int64 {
	t.Helper()
	res, err := central.Exec(
		`INSERT INTO "TENANT"(tenant_uuid, name, db_path, status, created_at) VALUES(?,?,?,?,?)`,
		uuid, name, "/var/lib/influence/"+uuid+".db", "active", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// seedUser inserts a user with a hashed password and returns its id.
func seedUser(t *testing.T, central *sql.DB, tenantID int64, username, password string) int64 {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	res, err := central.Exec(
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?,?,?,?,?)`,
		tenantID, username, username, hash, "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// TestLoginSuccessCreatesSession asserts valid credentials establish a session
// resolved to the user's single tenant (Req 3.1, 1.3, 1.4) and that the SESSION
// row is persisted with matching timestamps.
func TestLoginSuccessCreatesSession(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2")

	fixed := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	svc := NewAuthService(conns).withClock(func() time.Time { return fixed })

	sess, err := svc.Login(context.Background(), "alice", "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.Token == "" {
		t.Fatal("session token should not be empty")
	}
	if sess.UserID != data.UserID(userID) {
		t.Fatalf("session user = %d, want %d", sess.UserID, userID)
	}
	if sess.TenantID != data.TenantID(tenantID) {
		t.Fatalf("session tenant = %d, want %d", sess.TenantID, tenantID)
	}

	// The SESSION row must exist and reference the user.
	var (
		gotUser              int64
		createdAt, lastSeen  string
	)
	err = central.QueryRow(
		`SELECT user_id, created_at, last_seen_at FROM "SESSION" WHERE token = ?`, sess.Token,
	).Scan(&gotUser, &createdAt, &lastSeen)
	if err != nil {
		t.Fatalf("select session: %v", err)
	}
	if gotUser != userID {
		t.Fatalf("session row user = %d, want %d", gotUser, userID)
	}
	if createdAt != lastSeen {
		t.Fatalf("new session created_at (%q) and last_seen_at (%q) should be equal", createdAt, lastSeen)
	}
}

// TestLoginWrongPassword asserts a wrong password is denied with the generic
// invalid-credentials error and creates no session (Req 3.2).
func TestLoginWrongPassword(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2")

	svc := NewAuthService(conns)
	_, err := svc.Login(context.Background(), "alice", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}

	var count int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "SESSION"`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed login must create no session, found %d", count)
	}
}

// TestLoginUnknownUser asserts an unknown username returns the same error as a
// wrong password, so the two are indistinguishable (Req 3.2).
func TestLoginUnknownUser(t *testing.T) {
	conns := newTestConns(t)
	svc := NewAuthService(conns)
	_, err := svc.Login(context.Background(), "ghost", "whatever")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for unknown user, got %v", err)
	}
}

// TestLoginDeactivatedDenied asserts a deactivated account is denied even with
// the correct password (Req 5.4).
func TestLoginDeactivatedDenied(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2")
	if _, err := central.Exec(`UPDATE "USER" SET deactivated = 1 WHERE username = 'alice'`); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	svc := NewAuthService(conns)
	_, err := svc.Login(context.Background(), "alice", "hunter2")
	if !errors.Is(err, ErrAccountDeactivated) {
		t.Fatalf("expected ErrAccountDeactivated, got %v", err)
	}
}

// TestLoginLockedAccountDenied asserts a login is refused while a lockout window
// is active, even with the correct password (Req 3.3 lockout honored here; the
// window is set by task 5.2).
func TestLoginLockedAccountDenied(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2")

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(10 * time.Minute).Format(time.RFC3339Nano)
	if _, err := central.Exec(`UPDATE "USER" SET locked_until = ? WHERE username = 'alice'`, future); err != nil {
		t.Fatalf("set lock: %v", err)
	}

	svc := NewAuthService(conns).withClock(func() time.Time { return now })
	_, err := svc.Login(context.Background(), "alice", "hunter2")
	if !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("expected ErrAccountLocked, got %v", err)
	}
}

// TestLoginExpiredLockAllowsLogin asserts an expired lockout window no longer
// blocks a valid login, and that a successful login clears the lock and counter
// (Req 3.3 reset-on-success).
func TestLoginExpiredLockAllowsLogin(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2")

	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-1 * time.Minute).Format(time.RFC3339Nano)
	if _, err := central.Exec(
		`UPDATE "USER" SET locked_until = ?, failed_attempts = 4 WHERE username = 'alice'`, past,
	); err != nil {
		t.Fatalf("set expired lock: %v", err)
	}

	svc := NewAuthService(conns).withClock(func() time.Time { return now })
	if _, err := svc.Login(context.Background(), "alice", "hunter2"); err != nil {
		t.Fatalf("login after expired lock should succeed: %v", err)
	}

	var failed int
	var locked sql.NullString
	if err := central.QueryRow(
		`SELECT failed_attempts, locked_until FROM "USER" WHERE username = 'alice'`,
	).Scan(&failed, &locked); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if failed != 0 {
		t.Fatalf("failed_attempts should reset to 0 on success, got %d", failed)
	}
	if locked.Valid {
		t.Fatalf("locked_until should be cleared on success, got %q", locked.String)
	}
}

// TestSessionTokensAreUnique asserts two logins produce distinct high-entropy
// tokens.
func TestSessionTokensAreUnique(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2")

	svc := NewAuthService(conns)
	s1, err := svc.Login(context.Background(), "alice", "hunter2")
	if err != nil {
		t.Fatalf("login 1: %v", err)
	}
	s2, err := svc.Login(context.Background(), "alice", "hunter2")
	if err != nil {
		t.Fatalf("login 2: %v", err)
	}
	if s1.Token == s2.Token {
		t.Fatal("session tokens must be unique per login")
	}
}
