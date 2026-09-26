package service

// Unit tests for the admin User-lifecycle service (Req 32.2, 5.1, 5.4, 32.6,
// 32.7). They stand up a real Central_Directory over the shared user/group test
// env and assert:
//
//   - Create validates the password, stores a hash (never plaintext), and adds
//     the initial membership atomically (Req 32.2, 4.1).
//   - Create is transactional: a create that fails on an out-of-tenant initial
//     Group leaves no USER row behind (Req 32.7).
//   - List is scoped to the caller's tenant (Req 32.6).
//   - Deactivate/Reactivate toggle the flag (Req 5.4) and reject a User outside
//     the caller's tenant with ErrUserNotAccessible (Req 32.6).

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/influence/influence/internal/data"
)

// seedSecondTenant inserts a second active tenant in the Central_Directory and
// returns its id, so cross-tenant isolation can be exercised. The tenant needs
// no database for these Central-only tests.
func seedSecondTenant(t *testing.T, central *sql.DB) data.TenantID {
	t.Helper()
	res, err := central.Exec(
		`INSERT INTO "TENANT"(tenant_uuid, name, db_path, status, created_at) VALUES(?,?,?,?,?)`,
		"uuid-other-"+t.Name(), "Other", "unused", "active", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("second tenant id: %v", err)
	}
	return data.TenantID(id)
}

// countUsers returns how many USER rows exist for a tenant.
func countUsers(t *testing.T, central *sql.DB, tenantID data.TenantID) int {
	t.Helper()
	var n int
	if err := central.QueryRow(
		`SELECT COUNT(*) FROM "USER" WHERE tenant_id = ?`, int64(tenantID),
	).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

func TestCreateUserStoresHashAndMembership(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	group := seedGroup(t, central, tenantID, "Editors", false)
	svc := NewAdminUserService(conns)

	const pw = "Sup3rSecret!pw"
	u, err := svc.CreateUser(context.Background(), rc, "alice", "Alice", pw, group)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if u.ID == 0 || u.Username != "alice" || u.DisplayName != "Alice" || u.Deactivated {
		t.Fatalf("created user = %+v, want id set, alice/Alice, not deactivated", u)
	}

	// The stored credential is a hash that verifies the password, never the
	// plaintext (Req 3.4).
	var stored string
	if err := central.QueryRow(`SELECT password_hash FROM "USER" WHERE id = ?`, u.ID).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if stored == pw {
		t.Fatal("password stored in plaintext")
	}
	ok, err := VerifyPassword(pw, stored)
	if err != nil || !ok {
		t.Fatalf("stored hash does not verify password: ok=%v err=%v", ok, err)
	}

	// The initial membership was created (Req 4.1).
	var members int
	if err := central.QueryRow(
		`SELECT COUNT(*) FROM "GROUP_MEMBERSHIP" WHERE user_id = ? AND group_id = ?`,
		u.ID, int64(group),
	).Scan(&members); err != nil {
		t.Fatalf("count membership: %v", err)
	}
	if members != 1 {
		t.Fatalf("membership count = %d, want 1", members)
	}
}

func TestCreateUserDefaultsDisplayNameToUsername(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	group := seedGroup(t, central, tenantID, "Editors", false)
	svc := NewAdminUserService(conns)

	u, err := svc.CreateUser(context.Background(), rc, "bob", "", "Sup3rSecret!pw", group)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if u.DisplayName != "bob" {
		t.Fatalf("display name = %q, want defaulted to username", u.DisplayName)
	}
}

func TestCreateUserRejectsWeakPassword(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	group := seedGroup(t, central, tenantID, "Editors", false)
	svc := NewAdminUserService(conns)

	_, err := svc.CreateUser(context.Background(), rc, "weak", "Weak", "short", group)
	if !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("err = %v, want ErrPasswordPolicy", err)
	}
	if n := countUsers(t, central, tenantID); n != 0 {
		t.Fatalf("user count = %d, want 0 (create must not persist on weak password)", n)
	}
}

func TestCreateUserRejectsDuplicateUsername(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	group := seedGroup(t, central, tenantID, "Editors", false)
	svc := NewAdminUserService(conns)

	const pw = "Sup3rSecret!pw"
	if _, err := svc.CreateUser(context.Background(), rc, "carol", "Carol", pw, group); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.CreateUser(context.Background(), rc, "carol", "Carol2", pw, group)
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("err = %v, want ErrUsernameTaken", err)
	}
	if n := countUsers(t, central, tenantID); n != 1 {
		t.Fatalf("user count = %d, want 1 (duplicate must not create a second row)", n)
	}
}

// TestCreateUserRollsBackOnOutOfTenantGroup asserts the transactional guarantee
// of Req 32.7: when the initial Group is not in the caller's tenant, the create
// fails with TENANT_ISOLATION and leaves no USER row behind.
func TestCreateUserRollsBackOnOutOfTenantGroup(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	otherTenant := seedSecondTenant(t, central)
	// A group that belongs to the OTHER tenant, not the caller's.
	foreignGroup := seedGroup(t, central, otherTenant, "Foreigners", false)
	svc := NewAdminUserService(conns)

	_, err := svc.CreateUser(context.Background(), rc, "mallory", "Mallory", "Sup3rSecret!pw", foreignGroup)
	if !errors.Is(err, ErrGroupNotAccessible) {
		t.Fatalf("err = %v, want ErrGroupNotAccessible", err)
	}
	if n := countUsers(t, central, tenantID); n != 0 {
		t.Fatalf("user count = %d, want 0 (create must roll back on out-of-tenant group)", n)
	}
}

func TestCreateUserRejectsMissingGroup(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	svc := NewAdminUserService(conns)

	_, err := svc.CreateUser(context.Background(), rc, "nobody", "Nobody", "Sup3rSecret!pw", data.GroupID(99999))
	if !errors.Is(err, ErrGroupNotAccessible) {
		t.Fatalf("err = %v, want ErrGroupNotAccessible for a nonexistent group", err)
	}
	if n := countUsers(t, central, tenantID); n != 0 {
		t.Fatalf("user count = %d, want 0", n)
	}
}

func TestCreateUserRejectsEmptyUsername(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	group := seedGroup(t, central, tenantID, "Editors", false)
	svc := NewAdminUserService(conns)

	_, err := svc.CreateUser(context.Background(), rc, "", "", "Sup3rSecret!pw", group)
	if !errors.Is(err, ErrUsernameRequired) {
		t.Fatalf("err = %v, want ErrUsernameRequired", err)
	}
}

// TestListUsersScopedToTenant asserts the listing returns only the caller's
// tenant's users and never another tenant's (Req 32.6).
func TestListUsersScopedToTenant(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	otherTenant := seedSecondTenant(t, central)

	// Two users in the caller's tenant, one in another tenant.
	seedUser(t, central, int64(tenantID), "alice", "pw")
	seedUser(t, central, int64(tenantID), "bob", "pw")
	seedUser(t, central, int64(otherTenant), "eve", "pw")

	svc := NewAdminUserService(conns)
	users, err := svc.ListUsers(context.Background(), rc)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("listed %d users, want 2 (only the caller's tenant)", len(users))
	}
	// Ordered by username: alice then bob; eve (other tenant) must be absent.
	if users[0].Username != "alice" || users[1].Username != "bob" {
		t.Fatalf("usernames = [%q %q], want [alice bob]", users[0].Username, users[1].Username)
	}
}

func TestDeactivateReactivateTogglesFlag(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	uid := seedUser(t, central, int64(tenantID), "alice", "pw")
	svc := NewAdminUserService(conns)

	if err := svc.Deactivate(context.Background(), rc, uid); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if !readDeactivated(t, central, uid) {
		t.Fatal("user should be deactivated after Deactivate")
	}

	if err := svc.Reactivate(context.Background(), rc, uid); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if readDeactivated(t, central, uid) {
		t.Fatal("user should be active after Reactivate")
	}
}

// TestDeactivateRejectsCrossTenant asserts a User in another tenant cannot be
// deactivated through a caller scoped to a different tenant (Req 32.6), and the
// target row is left untouched.
func TestDeactivateRejectsCrossTenant(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	_ = tenantID
	otherTenant := seedSecondTenant(t, central)
	foreignUser := seedUser(t, central, int64(otherTenant), "eve", "pw")

	svc := NewAdminUserService(conns)
	err := svc.Deactivate(context.Background(), rc, foreignUser)
	if !errors.Is(err, ErrUserNotAccessible) {
		t.Fatalf("err = %v, want ErrUserNotAccessible", err)
	}
	if readDeactivated(t, central, foreignUser) {
		t.Fatal("cross-tenant user must be left untouched")
	}
}

func TestReactivateRejectsCrossTenant(t *testing.T) {
	conns, central, _, rc := newUserGroupEnv(t)
	otherTenant := seedSecondTenant(t, central)
	foreignUser := seedUser(t, central, int64(otherTenant), "eve", "pw")

	svc := NewAdminUserService(conns)
	err := svc.Reactivate(context.Background(), rc, foreignUser)
	if !errors.Is(err, ErrUserNotAccessible) {
		t.Fatalf("err = %v, want ErrUserNotAccessible", err)
	}
}

// readDeactivated reads the deactivated flag for a user directly.
func readDeactivated(t *testing.T, central *sql.DB, userID int64) bool {
	t.Helper()
	var d int
	if err := central.QueryRow(`SELECT deactivated FROM "USER" WHERE id = ?`, userID).Scan(&d); err != nil {
		t.Fatalf("read deactivated: %v", err)
	}
	return d != 0
}
