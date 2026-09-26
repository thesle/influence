package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/influence/influence/internal/data"
)

// newUserGroupEnv builds a real ConnManager over an in-memory Central_Directory
// plus one active tenant whose database is a shared-cache in-memory SQLite file.
// It returns the ConnManager, the central handle, the tenant id, and a
// RequestContext scoped to that tenant so grant operations resolve the tenant
// through the manager exactly as production code does.
//
// The tenant db_path recorded in the registry is a shared-cache in-memory DSN;
// a handle to that DSN is opened and migrated here and kept alive for the test
// so the shared-cache database persists when the ConnManager opens it by path.
func newUserGroupEnv(t *testing.T) (data.ConnManager, *sql.DB, data.TenantID, data.RequestContext) {
	t.Helper()
	ctx := context.Background()

	central, err := sql.Open(data.DriverName, "file:central-"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open central: %v", err)
	}
	central.SetMaxOpenConns(1)
	if err := data.MigrateCentral(ctx, central); err != nil {
		t.Fatalf("migrate central: %v", err)
	}

	// The tenant database lives at this DSN. openTenantPool opens the db_path
	// verbatim, so a shared-cache in-memory DSN works as the "file".
	tenantDSN := "file:tenant-" + t.Name() + "?mode=memory&cache=shared&_pragma=foreign_keys(1)"
	tenantDB, err := sql.Open(data.DriverName, tenantDSN)
	if err != nil {
		t.Fatalf("open tenant: %v", err)
	}
	tenantDB.SetMaxOpenConns(1)
	if err := data.MigrateTenant(ctx, tenantDB); err != nil {
		t.Fatalf("migrate tenant: %v", err)
	}

	res, err := central.Exec(
		`INSERT INTO "TENANT"(tenant_uuid, name, db_path, status, created_at) VALUES(?,?,?,?,?)`,
		"uuid-ug-"+t.Name(), "Org", tenantDSN, "active", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	tid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("tenant id: %v", err)
	}

	conns := data.NewConnManager(central)
	t.Cleanup(func() {
		_ = conns.Close()
		_ = tenantDB.Close()
	})

	rc := data.NewRequestContext(1, data.TenantID(tid), nil, nil)
	return conns, central, data.TenantID(tid), rc
}

// seedGroup inserts a group in the tenant and returns its id.
func seedGroup(t *testing.T, central *sql.DB, tenantID data.TenantID, name string, admin bool) data.GroupID {
	t.Helper()
	isAdmin := 0
	if admin {
		isAdmin = 1
	}
	res, err := central.Exec(
		`INSERT INTO "GROUP"(tenant_id, name, is_admin) VALUES(?,?,?)`,
		int64(tenantID), name, isAdmin,
	)
	if err != nil {
		t.Fatalf("insert group %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("group id: %v", err)
	}
	return data.GroupID(id)
}

// seedSphere inserts a sphere in the tenant DB and returns its surrogate id.
func seedSphere(t *testing.T, conns data.ConnManager, rc data.RequestContext, name string) data.SphereID {
	t.Helper()
	tdb, err := conns.Tenant(context.Background(), rc)
	if err != nil {
		t.Fatalf("resolve tenant: %v", err)
	}
	res, err := tdb.DB().Exec(
		`INSERT INTO sphere(record_id, name, created_at) VALUES(?,?,?)`,
		"rec-"+name, name, "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert sphere %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("sphere id: %v", err)
	}
	return data.SphereID(id)
}

// membershipCount reports how many groups the user belongs to.
func membershipCount(t *testing.T, central *sql.DB, user data.UserID) int {
	t.Helper()
	var n int
	if err := central.QueryRow(
		`SELECT COUNT(*) FROM "GROUP_MEMBERSHIP" WHERE user_id = ?`, int64(user),
	).Scan(&n); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	return n
}

// TestAddMembershipIsIdempotent verifies adding a membership twice leaves a
// single edge and no error (the composite PK makes the second a no-op).
func TestAddMembershipIsIdempotent(t *testing.T) {
	conns, central, tenantID, _ := newUserGroupEnv(t)
	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))
	g := seedGroup(t, central, tenantID, "engineers", false)

	svc := NewUserGroupService(conns)
	for i := 0; i < 2; i++ {
		if err := svc.AddMembership(context.Background(), uid, g); err != nil {
			t.Fatalf("add membership #%d: %v", i, err)
		}
	}
	if got := membershipCount(t, central, uid); got != 1 {
		t.Fatalf("membership count = %d, want 1", got)
	}
}

// TestRemoveMembershipRejectsLastGroup verifies removing a user's only Group is
// rejected with ErrMinGroupMembership and leaves memberships unchanged (Req 4.1).
func TestRemoveMembershipRejectsLastGroup(t *testing.T) {
	conns, central, tenantID, _ := newUserGroupEnv(t)
	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))
	only := seedGroup(t, central, tenantID, "engineers", false)

	svc := NewUserGroupService(conns)
	if err := svc.AddMembership(context.Background(), uid, only); err != nil {
		t.Fatalf("add membership: %v", err)
	}

	err := svc.RemoveMembership(context.Background(), uid, only)
	if !errors.Is(err, ErrMinGroupMembership) {
		t.Fatalf("remove last membership err = %v, want ErrMinGroupMembership", err)
	}
	if got := membershipCount(t, central, uid); got != 1 {
		t.Fatalf("membership count after rejected removal = %d, want 1 (unchanged)", got)
	}
}

// TestRemoveMembershipAllowsWhenAnotherRemains verifies a membership can be
// removed while the user keeps at least one Group (Req 4.1).
func TestRemoveMembershipAllowsWhenAnotherRemains(t *testing.T) {
	conns, central, tenantID, _ := newUserGroupEnv(t)
	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))
	g1 := seedGroup(t, central, tenantID, "engineers", false)
	g2 := seedGroup(t, central, tenantID, "writers", false)

	svc := NewUserGroupService(conns)
	if err := svc.AddMembership(context.Background(), uid, g1); err != nil {
		t.Fatalf("add g1: %v", err)
	}
	if err := svc.AddMembership(context.Background(), uid, g2); err != nil {
		t.Fatalf("add g2: %v", err)
	}

	if err := svc.RemoveMembership(context.Background(), uid, g1); err != nil {
		t.Fatalf("remove g1: %v", err)
	}
	if got := membershipCount(t, central, uid); got != 1 {
		t.Fatalf("membership count = %d, want 1", got)
	}
	remaining, err := svc.Memberships(context.Background(), uid)
	if err != nil {
		t.Fatalf("memberships: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != g2 {
		t.Fatalf("remaining memberships = %v, want [%d]", remaining, g2)
	}
}

// TestRemoveMembershipMissingIsNoop verifies removing a membership the user does
// not have succeeds without altering the user's other memberships.
func TestRemoveMembershipMissingIsNoop(t *testing.T) {
	conns, central, tenantID, _ := newUserGroupEnv(t)
	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))
	held := seedGroup(t, central, tenantID, "engineers", false)
	notHeld := seedGroup(t, central, tenantID, "writers", false)

	svc := NewUserGroupService(conns)
	if err := svc.AddMembership(context.Background(), uid, held); err != nil {
		t.Fatalf("add held: %v", err)
	}

	if err := svc.RemoveMembership(context.Background(), uid, notHeld); err != nil {
		t.Fatalf("remove not-held membership err = %v, want nil", err)
	}
	if got := membershipCount(t, central, uid); got != 1 {
		t.Fatalf("membership count = %d, want 1 (unchanged)", got)
	}
}

// TestGrantSphereRejectsAccessNone verifies granting AccessNone is rejected: a
// grant must confer read or write (Req 4.2/4.3), and "no access" is expressed by
// revoking.
func TestGrantSphereRejectsAccessNone(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	g := seedGroup(t, central, tenantID, "engineers", false)
	sphere := seedSphere(t, conns, rc, "docs")

	svc := NewUserGroupService(conns)
	err := svc.GrantSphere(context.Background(), rc, sphere, g, data.AccessNone, false)
	if !errors.Is(err, ErrInvalidAccessLevel) {
		t.Fatalf("grant AccessNone err = %v, want ErrInvalidAccessLevel", err)
	}
}

// TestGrantAppliesToCurrentMembers verifies that granting a Group access to a
// Sphere confers that access on a current member, derived via EffectiveAccess
// (Req 4.3), and that reveal follows the grant flag (Req 4.4).
func TestGrantAppliesToCurrentMembers(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))
	g := seedGroup(t, central, tenantID, "engineers", false)
	sphere := seedSphere(t, conns, rc, "docs")

	svc := NewUserGroupService(conns)
	if err := svc.AddMembership(context.Background(), uid, g); err != nil {
		t.Fatalf("add membership: %v", err)
	}

	// Before any grant, the member has no access (Req 4.5).
	before, err := svc.EffectiveAccess(context.Background(), rc, uid, sphere, false, false)
	if err != nil {
		t.Fatalf("effective before: %v", err)
	}
	if before.Access != data.AccessNone || before.Reveal {
		t.Fatalf("effective before grant = %+v, want none/no-reveal", before)
	}

	if err := svc.GrantSphere(context.Background(), rc, sphere, g, data.AccessWrite, true); err != nil {
		t.Fatalf("grant: %v", err)
	}

	after, err := svc.EffectiveAccess(context.Background(), rc, uid, sphere, false, false)
	if err != nil {
		t.Fatalf("effective after: %v", err)
	}
	if after.Access != data.AccessWrite {
		t.Fatalf("effective access after grant = %v, want write", after.Access)
	}
	if !after.Reveal {
		t.Fatalf("reveal after grant with reveal=true = false, want true")
	}
}

// TestGrantUpsertUpdatesExisting verifies re-granting an existing (Sphere,Group)
// pair updates the access level and reveal flag rather than erroring.
func TestGrantUpsertUpdatesExisting(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))
	g := seedGroup(t, central, tenantID, "engineers", false)
	sphere := seedSphere(t, conns, rc, "docs")

	svc := NewUserGroupService(conns)
	mustAdd(t, svc, uid, g)

	if err := svc.GrantSphere(context.Background(), rc, sphere, g, data.AccessRead, false); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	if err := svc.GrantSphere(context.Background(), rc, sphere, g, data.AccessWrite, true); err != nil {
		t.Fatalf("upsert grant: %v", err)
	}

	got, err := svc.EffectiveAccess(context.Background(), rc, uid, sphere, false, false)
	if err != nil {
		t.Fatalf("effective: %v", err)
	}
	if got.Access != data.AccessWrite || !got.Reveal {
		t.Fatalf("effective after upsert = %+v, want write/reveal", got)
	}

	// Exactly one grant row should exist for the pair.
	if n := grantRowCount(t, conns, rc, sphere, g); n != 1 {
		t.Fatalf("grant rows = %d, want 1 (upsert, not insert)", n)
	}
}

// TestRevokeRemovesAccessUnlessRetainedByAnotherGroup verifies that revoking a
// Group's grant removes the member's access only when no other Group still
// grants it (Req 4.6): with two granting Groups, revoking one leaves access via
// the other; revoking the second removes it entirely.
func TestRevokeRemovesAccessUnlessRetainedByAnotherGroup(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))
	g1 := seedGroup(t, central, tenantID, "engineers", false)
	g2 := seedGroup(t, central, tenantID, "writers", false)
	sphere := seedSphere(t, conns, rc, "docs")

	svc := NewUserGroupService(conns)
	mustAdd(t, svc, uid, g1)
	mustAdd(t, svc, uid, g2)

	// Both groups grant read on the Sphere.
	if err := svc.GrantSphere(context.Background(), rc, sphere, g1, data.AccessRead, false); err != nil {
		t.Fatalf("grant g1: %v", err)
	}
	if err := svc.GrantSphere(context.Background(), rc, sphere, g2, data.AccessRead, false); err != nil {
		t.Fatalf("grant g2: %v", err)
	}

	// Revoke g1's grant: the member keeps read access through g2 (Req 4.6).
	if err := svc.RevokeSphere(context.Background(), rc, sphere, g1); err != nil {
		t.Fatalf("revoke g1: %v", err)
	}
	retained, err := svc.EffectiveAccess(context.Background(), rc, uid, sphere, false, false)
	if err != nil {
		t.Fatalf("effective after revoke g1: %v", err)
	}
	if retained.Access != data.AccessRead {
		t.Fatalf("access after revoking one of two grants = %v, want read (retained via other group)", retained.Access)
	}

	// Revoke g2's grant: no Group grants the Sphere now, so access is gone.
	if err := svc.RevokeSphere(context.Background(), rc, sphere, g2); err != nil {
		t.Fatalf("revoke g2: %v", err)
	}
	gone, err := svc.EffectiveAccess(context.Background(), rc, uid, sphere, false, false)
	if err != nil {
		t.Fatalf("effective after revoke g2: %v", err)
	}
	if gone.Access != data.AccessNone {
		t.Fatalf("access after revoking both grants = %v, want none", gone.Access)
	}
}

// TestEffectiveAccessUnionMostPermissive verifies that when two of a user's
// Groups grant different levels on the same Sphere, the effective access is the
// most-permissive of them (Req 4.3), matching the PolicyEngine union.
func TestEffectiveAccessUnionMostPermissive(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))
	readers := seedGroup(t, central, tenantID, "readers", false)
	writers := seedGroup(t, central, tenantID, "writers", false)
	sphere := seedSphere(t, conns, rc, "docs")

	svc := NewUserGroupService(conns)
	mustAdd(t, svc, uid, readers)
	mustAdd(t, svc, uid, writers)
	if err := svc.GrantSphere(context.Background(), rc, sphere, readers, data.AccessRead, false); err != nil {
		t.Fatalf("grant readers: %v", err)
	}
	if err := svc.GrantSphere(context.Background(), rc, sphere, writers, data.AccessWrite, false); err != nil {
		t.Fatalf("grant writers: %v", err)
	}

	got, err := svc.EffectiveAccess(context.Background(), rc, uid, sphere, false, false)
	if err != nil {
		t.Fatalf("effective: %v", err)
	}
	if got.Access != data.AccessWrite {
		t.Fatalf("union access = %v, want write (most permissive)", got.Access)
	}
}

// TestEffectiveAccessAdminImplicitGrant verifies an admin caller has write+reveal
// on a Sphere with no explicit grant (Req 4.7), and a deactivated user has none
// even with a grant (Req 5.4), threaded through EffectiveAccess.
func TestEffectiveAccessAdminAndDeactivated(t *testing.T) {
	conns, central, tenantID, rc := newUserGroupEnv(t)
	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))
	g := seedGroup(t, central, tenantID, "engineers", false)
	sphere := seedSphere(t, conns, rc, "docs")

	svc := NewUserGroupService(conns)
	mustAdd(t, svc, uid, g)

	admin, err := svc.EffectiveAccess(context.Background(), rc, uid, sphere, true, false)
	if err != nil {
		t.Fatalf("effective admin: %v", err)
	}
	if admin.Access != data.AccessWrite || !admin.Reveal {
		t.Fatalf("admin effective = %+v, want write/reveal", admin)
	}

	// Even with a write+reveal grant, a deactivated user gets nothing.
	if err := svc.GrantSphere(context.Background(), rc, sphere, g, data.AccessWrite, true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	deact, err := svc.EffectiveAccess(context.Background(), rc, uid, sphere, false, true)
	if err != nil {
		t.Fatalf("effective deactivated: %v", err)
	}
	if deact.Access != data.AccessNone || deact.Reveal {
		t.Fatalf("deactivated effective = %+v, want none/no-reveal", deact)
	}
}

// mustAdd adds a membership or fails the test.
func mustAdd(t *testing.T, svc *UserGroupService, u data.UserID, g data.GroupID) {
	t.Helper()
	if err := svc.AddMembership(context.Background(), u, g); err != nil {
		t.Fatalf("add membership: %v", err)
	}
}

// grantRowCount counts SPHERE_GRANT rows for a (sphere, group) pair in the
// caller's tenant.
func grantRowCount(t *testing.T, conns data.ConnManager, rc data.RequestContext, sphere data.SphereID, group data.GroupID) int {
	t.Helper()
	tdb, err := conns.Tenant(context.Background(), rc)
	if err != nil {
		t.Fatalf("resolve tenant: %v", err)
	}
	var n int
	if err := tdb.DB().QueryRow(
		`SELECT COUNT(*) FROM sphere_grant WHERE sphere_id = ? AND group_id = ?`,
		int64(sphere), int64(group),
	).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n
}
