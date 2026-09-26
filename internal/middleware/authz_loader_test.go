package middleware

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/service"
)

// openMem opens a named shared-cache in-memory SQLite database. The returned
// handle must stay open for the lifetime of the test so the shared database is
// not discarded; the caller registers a cleanup.
func openMem(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open(data.DriverName, "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	// A single connection keeps the shared in-memory database alive and avoids
	// cross-connection visibility surprises for these small tests.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// loaderFixture wires a real Central_Directory and a real tenant database
// through a ConnManager so the AuthzLoader can be exercised end to end against
// SQLite, matching how it runs in production.
type loaderFixture struct {
	conns    data.ConnManager
	central  *sql.DB
	tenantID data.TenantID
	userID   data.UserID
}

// newLoaderFixture builds a Central DB (migrated + seeded with a tenant, user,
// and groups) and a tenant DB (migrated + seeded with sphere grants), and
// returns a ConnManager over them. tenantDBPath is the shared in-memory DSN
// registered in TENANT.db_path so the connection manager reopens the same
// database.
func newLoaderFixture(t *testing.T) *loaderFixture {
	t.Helper()
	ctx := context.Background()

	central := openMem(t, "central_authz")
	if err := data.MigrateCentral(ctx, central); err != nil {
		t.Fatalf("migrate central: %v", err)
	}

	tenantDSN := "file:tenant_authz?mode=memory&cache=shared"
	// Keep a live handle so the shared in-memory tenant DB persists; the
	// ConnManager will open the same DSN via the registry.
	tenantKeepAlive := openMem(t, "tenant_authz")
	if err := data.MigrateTenant(ctx, tenantKeepAlive); err != nil {
		t.Fatalf("migrate tenant: %v", err)
	}

	// Seed the tenant registry row (active) so the registry resolves its path,
	// then the user, groups, and memberships.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := central.ExecContext(ctx,
		`INSERT INTO "TENANT"(tenant_uuid, name, db_path, status, created_at)
		 VALUES('uuid-authz', 'Authz Tenant', ?, 'active', ?)`,
		tenantDSN, now,
	)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	tenantID, _ := res.LastInsertId()

	userRes, err := central.ExecContext(ctx,
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?, 'alice', 'Alice', 'x', ?)`,
		tenantID, now,
	)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	userID, _ := userRes.LastInsertId()

	// Two ordinary groups (readers=10-ish, writers) plus one Admin_Group.
	readersID := seedGroup(t, ctx, central, tenantID, "readers", false)
	writersID := seedGroup(t, ctx, central, tenantID, "writers", false)
	adminID := seedGroup(t, ctx, central, tenantID, "admins", true)

	// The fixture user belongs to readers and writers (not admin by default).
	addMembership(t, ctx, central, userID, readersID)
	addMembership(t, ctx, central, userID, writersID)

	// Seed the two spheres the grants reference so the FK holds. Explicit ids
	// (5, 6) are used so the grant rows and assertions can reference them.
	seedSphere(t, ctx, tenantKeepAlive, 5, "Sphere Five")
	seedSphere(t, ctx, tenantKeepAlive, 6, "Sphere Six")

	// Sphere grants in the tenant DB: readers get read on sphere 5; writers get
	// write+reveal on sphere 5 and read on sphere 6; the admin group gets a
	// grant on sphere 6 that the user should NOT see (not a member).
	seedGrant(t, ctx, tenantKeepAlive, 5, readersID, "read", false)
	seedGrant(t, ctx, tenantKeepAlive, 5, writersID, "write", true)
	seedGrant(t, ctx, tenantKeepAlive, 6, writersID, "read", false)
	seedGrant(t, ctx, tenantKeepAlive, 6, adminID, "write", true)

	conns := data.NewConnManager(central)
	t.Cleanup(func() { _ = conns.Close() })

	return &loaderFixture{
		conns:    conns,
		central:  central,
		tenantID: data.TenantID(tenantID),
		userID:   data.UserID(userID),
	}
}

func seedGroup(t *testing.T, ctx context.Context, central *sql.DB, tenantID int64, name string, admin bool) int64 {
	t.Helper()
	isAdmin := 0
	if admin {
		isAdmin = 1
	}
	res, err := central.ExecContext(ctx,
		`INSERT INTO "GROUP"(tenant_id, name, is_admin) VALUES(?,?,?)`,
		tenantID, name, isAdmin,
	)
	if err != nil {
		t.Fatalf("seed group %s: %v", name, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func addMembership(t *testing.T, ctx context.Context, central *sql.DB, userID, groupID int64) {
	t.Helper()
	if _, err := central.ExecContext(ctx,
		`INSERT INTO "GROUP_MEMBERSHIP"(user_id, group_id) VALUES(?,?)`,
		userID, groupID,
	); err != nil {
		t.Fatalf("add membership: %v", err)
	}
}

func seedSphere(t *testing.T, ctx context.Context, tenant *sql.DB, id int64, name string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tenant.ExecContext(ctx,
		`INSERT INTO sphere(id, record_id, name, created_at) VALUES(?,?,?,?)`,
		id, name, name, now,
	); err != nil {
		t.Fatalf("seed sphere %d: %v", id, err)
	}
}

func seedGrant(t *testing.T, ctx context.Context, tenant *sql.DB, sphereID, groupID int64, access string, reveal bool) {
	t.Helper()
	r := 0
	if reveal {
		r = 1
	}
	if _, err := tenant.ExecContext(ctx,
		`INSERT INTO sphere_grant(sphere_id, group_id, access, reveal) VALUES(?,?,?,?)`,
		sphereID, groupID, access, r,
	); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
}

// setDeactivated flips the fixture user's deactivated flag.
func (f *loaderFixture) setDeactivated(t *testing.T, on bool) {
	t.Helper()
	v := 0
	if on {
		v = 1
	}
	if _, err := f.central.ExecContext(context.Background(),
		`UPDATE "USER" SET deactivated = ? WHERE id = ?`, v, int64(f.userID),
	); err != nil {
		t.Fatalf("set deactivated: %v", err)
	}
}

// makeAdmin adds the fixture user to the Admin_Group.
func (f *loaderFixture) makeAdmin(t *testing.T) {
	t.Helper()
	var adminID int64
	if err := f.central.QueryRowContext(context.Background(),
		`SELECT id FROM "GROUP" WHERE name = 'admins'`,
	).Scan(&adminID); err != nil {
		t.Fatalf("find admin group: %v", err)
	}
	addMembership(t, context.Background(), f.central, int64(f.userID), adminID)
}

func (f *loaderFixture) rc() data.RequestContext {
	return data.NewRequestContext(f.userID, f.tenantID, nil, nil)
}

// TestAuthzLoader_LoadsGroupsAndGrants asserts the loader reads the user's
// Group memberships from Central and only the SPHERE_GRANT rows for those Groups
// from the tenant DB — the admin group's grant on sphere 6 is excluded because
// the user is not a member.
func TestAuthzLoader_LoadsGroupsAndGrants(t *testing.T) {
	f := newLoaderFixture(t)
	loader := NewAuthzLoader(f.conns)

	groups, authz, err := loader.Load(context.Background(), f.rc())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(groups) != 2 {
		t.Errorf("groups = %v, want 2 (readers, writers)", groups)
	}
	if authz.Admin {
		t.Error("Admin = true, want false (user not in Admin_Group)")
	}
	if authz.Deactivated {
		t.Error("Deactivated = true, want false")
	}

	// The user's grants: read on 5 (readers), write+reveal on 5 (writers), read
	// on 6 (writers). The admin group's write+reveal on sphere 6 must be absent.
	var sphere6Writes int
	for _, g := range authz.Grants {
		if g.Sphere == 6 && g.Access == data.AccessWrite {
			sphere6Writes++
		}
	}
	if sphere6Writes != 0 {
		t.Errorf("found %d write grants on sphere 6, want 0 (admin group's grant must be excluded)", sphere6Writes)
	}
	if len(authz.Grants) != 3 {
		t.Errorf("grants = %d, want 3 (the user's own group grants only)", len(authz.Grants))
	}

	// Feed through the PolicyEngine to confirm the effective decision matches
	// the union rule the middleware relies on.
	eng := service.NewPolicyEngine()
	access, _ := eng.CanAccessSphere(f.rc(), authz, 5)
	if access != data.AccessWrite {
		t.Errorf("effective access on sphere 5 = %v, want write", access)
	}
	if !eng.CanReveal(f.rc(), authz, 5) {
		t.Error("effective reveal on sphere 5 = false, want true")
	}
}

// TestAuthzLoader_Admin asserts membership in the Admin_Group surfaces as
// Admin = true.
func TestAuthzLoader_Admin(t *testing.T) {
	f := newLoaderFixture(t)
	f.makeAdmin(t)

	_, authz, err := NewAuthzLoader(f.conns).Load(context.Background(), f.rc())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !authz.Admin {
		t.Error("Admin = false, want true after joining Admin_Group")
	}
}

// TestAuthzLoader_Deactivated asserts the deactivated flag is loaded from
// Central.
func TestAuthzLoader_Deactivated(t *testing.T) {
	f := newLoaderFixture(t)
	f.setDeactivated(t, true)

	_, authz, err := NewAuthzLoader(f.conns).Load(context.Background(), f.rc())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !authz.Deactivated {
		t.Error("Deactivated = false, want true")
	}
}
