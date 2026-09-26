package service

// Unit tests for the BootstrapService (design.md — "First-Run Bootstrap — Req
// 31"; Property 33). They run against a real in-memory Central_Directory (via
// the shared newTestConns / seedTenant helpers) so the derived First_Run_State
// predicate and the single-transaction create run over the production SQL, not a
// fake.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/influence/influence/internal/data"
)

const goodBootstrapPassword = "Sup3rSecret!pw"

// countAdminMembersDB returns how many USERs in the tenant are members of one of
// the tenant's is_admin Groups — the raw predicate behind First_Run_State.
func countAdminMembersDB(t *testing.T, central *sql.DB, tenant int64) int {
	t.Helper()
	var n int
	if err := central.QueryRow(
		`SELECT COUNT(*) FROM "GROUP_MEMBERSHIP" gm
		   JOIN "USER"  u ON u.id = gm.user_id
		   JOIN "GROUP" g ON g.id = gm.group_id
		  WHERE g.tenant_id = ? AND u.tenant_id = ? AND g.is_admin = 1`,
		tenant, tenant,
	).Scan(&n); err != nil {
		t.Fatalf("count admin members: %v", err)
	}
	return n
}

// assertNoUsersAndStillFirstRun is the shared post-condition for a rejected
// create: no USER row exists in the tenant and it remains in First_Run_State.
func assertNoUsersAndStillFirstRun(t *testing.T, svc *BootstrapService, central *sql.DB, tenantID int64) {
	t.Helper()
	var n int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE tenant_id = ?`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected create persisted %d users, want 0", n)
	}
	needed, err := svc.FirstRunState(context.Background(), data.TenantID(tenantID))
	if err != nil {
		t.Fatalf("FirstRunState: %v", err)
	}
	if !needed {
		t.Fatal("tenant should still be in First_Run_State after a rejected create")
	}
}

// TestFirstRunStateTrueForFreshTenant asserts a tenant with no admin member is
// reported as still needing setup (Req 31.1).
func TestFirstRunStateTrueForFreshTenant(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")

	svc := NewBootstrapService(conns)
	needed, err := svc.FirstRunState(context.Background(), data.TenantID(tenantID))
	if err != nil {
		t.Fatalf("FirstRunState: %v", err)
	}
	if !needed {
		t.Fatal("fresh tenant should be in First_Run_State (needed=true)")
	}
}

// TestFirstRunStateFalseWhenAdminExists asserts that once a user is a member of
// the tenant's is_admin Group, the tenant is no longer in First_Run_State
// (Req 31.3). A non-admin member must NOT clear it.
func TestFirstRunStateFalseWhenAdminExists(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	svc := NewBootstrapService(conns)

	// A plain (non-admin) member does not clear First_Run_State.
	uid := seedUser(t, central, tenantID, "plain", "irrelevant-pw-123")
	if _, err := central.Exec(`INSERT INTO "GROUP"(tenant_id, name, is_admin) VALUES(?, 'editors', 0)`, tenantID); err != nil {
		t.Fatalf("insert plain group: %v", err)
	}
	var plainGID int64
	if err := central.QueryRow(`SELECT id FROM "GROUP" WHERE tenant_id = ? AND name='editors'`, tenantID).Scan(&plainGID); err != nil {
		t.Fatalf("lookup plain group: %v", err)
	}
	if _, err := central.Exec(`INSERT INTO "GROUP_MEMBERSHIP"(user_id, group_id) VALUES(?, ?)`, uid, plainGID); err != nil {
		t.Fatalf("insert plain membership: %v", err)
	}

	needed, err := svc.FirstRunState(context.Background(), data.TenantID(tenantID))
	if err != nil {
		t.Fatalf("FirstRunState: %v", err)
	}
	if !needed {
		t.Fatal("a non-admin member must not clear First_Run_State")
	}

	// Now add an admin member: First_Run_State must flip to false.
	if _, err := central.Exec(`INSERT INTO "GROUP"(tenant_id, name, is_admin) VALUES(?, 'admins', 1)`, tenantID); err != nil {
		t.Fatalf("insert admin group: %v", err)
	}
	var adminGID int64
	if err := central.QueryRow(`SELECT id FROM "GROUP" WHERE tenant_id = ? AND name='admins'`, tenantID).Scan(&adminGID); err != nil {
		t.Fatalf("lookup admin group: %v", err)
	}
	if _, err := central.Exec(`INSERT INTO "GROUP_MEMBERSHIP"(user_id, group_id) VALUES(?, ?)`, uid, adminGID); err != nil {
		t.Fatalf("insert admin membership: %v", err)
	}

	needed, err = svc.FirstRunState(context.Background(), data.TenantID(tenantID))
	if err != nil {
		t.Fatalf("FirstRunState: %v", err)
	}
	if needed {
		t.Fatal("an admin member must clear First_Run_State (needed=false)")
	}
}

// TestCreateBootstrapAdminClearsFirstRun asserts a successful create makes an
// Admin_Group member, stores an Argon2id hash (not the plaintext), and leaves
// the tenant no longer in First_Run_State (Req 31.2, 31.3, 3.4).
func TestCreateBootstrapAdminClearsFirstRun(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	svc := NewBootstrapService(conns)

	uid, err := svc.CreateBootstrapAdmin(context.Background(), data.TenantID(tenantID), "boss", goodBootstrapPassword)
	if err != nil {
		t.Fatalf("CreateBootstrapAdmin: %v", err)
	}
	if uid == 0 {
		t.Fatal("expected a non-zero created user id")
	}

	// The user is a member of an is_admin Group in the tenant.
	if got := countAdminMembersDB(t, central, tenantID); got != 1 {
		t.Fatalf("admin members = %d, want 1", got)
	}

	// The stored credential is an Argon2id hash, never the plaintext.
	var hash string
	if err := central.QueryRow(`SELECT password_hash FROM "USER" WHERE id = ?`, int64(uid)).Scan(&hash); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if hash == goodBootstrapPassword || !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("password_hash %q is not an Argon2id hash of the password", hash)
	}
	ok, err := VerifyPassword(goodBootstrapPassword, hash)
	if err != nil || !ok {
		t.Fatalf("stored hash does not verify against the password (ok=%v err=%v)", ok, err)
	}

	needed, err := svc.FirstRunState(context.Background(), data.TenantID(tenantID))
	if err != nil {
		t.Fatalf("FirstRunState: %v", err)
	}
	if needed {
		t.Fatal("tenant should be out of First_Run_State after create")
	}
}

// TestCreateBootstrapAdminSecondRejected asserts a second create against a tenant
// that already has an admin is rejected with ErrSetupComplete and creates no
// user (Req 31.4).
func TestCreateBootstrapAdminSecondRejected(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	svc := NewBootstrapService(conns)

	if _, err := svc.CreateBootstrapAdmin(context.Background(), data.TenantID(tenantID), "boss", goodBootstrapPassword); err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err := svc.CreateBootstrapAdmin(context.Background(), data.TenantID(tenantID), "usurper", goodBootstrapPassword)
	if !errors.Is(err, ErrSetupComplete) {
		t.Fatalf("second create error = %v, want ErrSetupComplete", err)
	}

	// No second user was created.
	var n int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE tenant_id = ?`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 1 {
		t.Fatalf("user count = %d, want 1 (second create must persist nothing)", n)
	}
}

// TestCreateBootstrapAdminReusesExistingAdminGroup asserts that when the tenant
// was provisioned with an is_admin Group already, the bootstrap joins the new
// user to that Group rather than creating a second one.
func TestCreateBootstrapAdminReusesExistingAdminGroup(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	if _, err := central.Exec(`INSERT INTO "GROUP"(tenant_id, name, is_admin) VALUES(?, 'Admins', 1)`, tenantID); err != nil {
		t.Fatalf("seed admin group: %v", err)
	}
	svc := NewBootstrapService(conns)

	if _, err := svc.CreateBootstrapAdmin(context.Background(), data.TenantID(tenantID), "boss", goodBootstrapPassword); err != nil {
		t.Fatalf("create: %v", err)
	}

	var groups int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "GROUP" WHERE tenant_id = ? AND is_admin = 1`, tenantID).Scan(&groups); err != nil {
		t.Fatalf("count admin groups: %v", err)
	}
	if groups != 1 {
		t.Fatalf("admin group count = %d, want 1 (existing group must be reused)", groups)
	}
}

// TestCreateBootstrapAdminWeakPasswordRejected asserts a password failing the
// shared policy is rejected with ErrPasswordPolicy and creates no user, leaving
// the tenant in First_Run_State (Req 31.5).
func TestCreateBootstrapAdminWeakPasswordRejected(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	svc := NewBootstrapService(conns)

	_, err := svc.CreateBootstrapAdmin(context.Background(), data.TenantID(tenantID), "boss", "short")
	if !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("weak password error = %v, want ErrPasswordPolicy", err)
	}
	assertNoUsersAndStillFirstRun(t, svc, central, tenantID)
}

// TestCreateBootstrapAdminEmptyUsernameRejected asserts an empty username is
// rejected with ErrUsernameRequired and creates no user (Req 31.5).
func TestCreateBootstrapAdminEmptyUsernameRejected(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	svc := NewBootstrapService(conns)

	_, err := svc.CreateBootstrapAdmin(context.Background(), data.TenantID(tenantID), "", goodBootstrapPassword)
	if !errors.Is(err, ErrUsernameRequired) {
		t.Fatalf("empty username error = %v, want ErrUsernameRequired", err)
	}
	assertNoUsersAndStillFirstRun(t, svc, central, tenantID)
}

// TestCreateBootstrapAdminTooLongUsernameRejected asserts a username longer than
// 100 characters is rejected with ErrUsernameTooLong and creates no user
// (Req 31.5); the exact 100-char boundary is accepted.
func TestCreateBootstrapAdminTooLongUsernameRejected(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	svc := NewBootstrapService(conns)

	long := strings.Repeat("a", 101)
	_, err := svc.CreateBootstrapAdmin(context.Background(), data.TenantID(tenantID), long, goodBootstrapPassword)
	if !errors.Is(err, ErrUsernameTooLong) {
		t.Fatalf("too-long username error = %v, want ErrUsernameTooLong", err)
	}
	assertNoUsersAndStillFirstRun(t, svc, central, tenantID)

	// The boundary length (exactly 100) is accepted.
	ok := strings.Repeat("b", 100)
	if _, err := svc.CreateBootstrapAdmin(context.Background(), data.TenantID(tenantID), ok, goodBootstrapPassword); err != nil {
		t.Fatalf("100-char username should be accepted, got %v", err)
	}
}

// TestConcurrentCreateExclusivity drives many setup requests at one tenant in
// parallel and asserts exactly one succeeds; every other is rejected with
// ErrSetupComplete and the tenant ends with a single admin user (design
// Property 33; Req 31.4). The in-transaction re-check is what makes this hold.
func TestConcurrentCreateExclusivity(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	svc := NewBootstrapService(conns)

	const racers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		other     []error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.CreateBootstrapAdmin(context.Background(), data.TenantID(tenantID), usernameFor(i), goodBootstrapPassword)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrSetupComplete):
				// expected loser
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) != 0 {
		t.Fatalf("unexpected errors from racers: %v", other)
	}
	if successes != 1 {
		t.Fatalf("successful creates = %d, want exactly 1", successes)
	}
	var users int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE tenant_id = ?`, tenantID).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Fatalf("user count = %d, want exactly 1 admin", users)
	}
	if got := countAdminMembersDB(t, central, tenantID); got != 1 {
		t.Fatalf("admin members = %d, want exactly 1", got)
	}
}

// TestResolveActiveTenant covers the tenant-mapping rule the setup handler uses:
// by uuid, the sole active tenant when none is supplied, and the not-found cases.
func TestResolveActiveTenant(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	svc := NewBootstrapService(conns)
	ctx := context.Background()

	// No tenants: empty uuid cannot resolve a sole tenant.
	if _, err := svc.ResolveActiveTenant(ctx, ""); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("no tenants, empty uuid: err = %v, want ErrTenantNotFound", err)
	}

	tid := seedTenant(t, central, "uuid-a", "Org A")

	// Sole tenant resolves with an empty uuid.
	got, err := svc.ResolveActiveTenant(ctx, "")
	if err != nil || int64(got) != tid {
		t.Fatalf("sole tenant: got %d err %v, want %d", got, err, tid)
	}
	// And by explicit uuid.
	got, err = svc.ResolveActiveTenant(ctx, "uuid-a")
	if err != nil || int64(got) != tid {
		t.Fatalf("by uuid: got %d err %v, want %d", got, err, tid)
	}
	// Unknown uuid is not found.
	if _, err := svc.ResolveActiveTenant(ctx, "nope"); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("unknown uuid: err = %v, want ErrTenantNotFound", err)
	}

	// A second active tenant makes an empty uuid ambiguous.
	seedTenant(t, central, "uuid-b", "Org B")
	if _, err := svc.ResolveActiveTenant(ctx, ""); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("two tenants, empty uuid: err = %v, want ErrTenantNotFound", err)
	}
}

func usernameFor(i int) string { return "admin" + string(rune('a'+i)) }
