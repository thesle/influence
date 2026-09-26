package service

// Unit tests for the per-Sphere Group grant half of Requirement 5.3 / 32.4:
// ListSphereGrants, GrantSphereByRecordID, and RevokeSphereByRecordID. They
// exercise the service directly over a real ConnManager (newUserGroupEnv) so the
// tenant scoping of both halves of a grant — the tenant-local Sphere resolved by
// Record_ID and the Central Group confirmed in-tenant — is proven at the service
// boundary, independent of the REST wiring.

import (
	"context"
	"errors"
	"testing"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// seedSphereRecordID inserts a Sphere in the caller's tenant with a freshly
// minted Record_ID and returns both the Record_ID and the surrogate id so a
// test can drive the Record_ID API and assert against the stored surrogate.
func seedSphereRecordID(t *testing.T, conns data.ConnManager, rc data.RequestContext, name string) (recordid.ID, data.SphereID) {
	t.Helper()
	rid := recordid.New()
	tdb, err := conns.Tenant(context.Background(), rc)
	if err != nil {
		t.Fatalf("resolve tenant: %v", err)
	}
	res, err := tdb.DB().Exec(
		`INSERT INTO sphere(record_id, name, created_at) VALUES(?,?,?)`,
		rid.Canonical(), name, "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert sphere %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("sphere id: %v", err)
	}
	return rid, data.SphereID(id)
}

// readGrant reads the stored access + reveal for a (sphere, group) pair from the
// caller's tenant, reporting ok=false when no grant row exists.
func readGrant(t *testing.T, conns data.ConnManager, rc data.RequestContext, sphere data.SphereID, group data.GroupID) (access string, reveal bool, ok bool) {
	t.Helper()
	tdb, err := conns.Tenant(context.Background(), rc)
	if err != nil {
		t.Fatalf("resolve tenant: %v", err)
	}
	var rev int
	err = tdb.DB().QueryRow(
		`SELECT access, reveal FROM sphere_grant WHERE sphere_id = ? AND group_id = ?`,
		int64(sphere), int64(group),
	).Scan(&access, &rev)
	if err != nil {
		return "", false, false
	}
	return access, rev != 0, true
}

// TestGrantSphereByRecordIDRoundTrip asserts a grant addressed by the Sphere
// Record_ID persists the resolved surrogate grant with the requested level and
// reveal, and a re-grant upserts (updates level + reveal in place).
func TestGrantSphereByRecordIDRoundTrip(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	ctx := context.Background()
	svc := NewUserGroupService(conns)

	group := seedGroup(t, central, tid, "editors", false)
	rid, surrogate := seedSphereRecordID(t, conns, rc, "Ops")

	if err := svc.GrantSphereByRecordID(ctx, rc, rid, group, data.AccessRead, false); err != nil {
		t.Fatalf("grant: %v", err)
	}
	access, reveal, ok := readGrant(t, conns, rc, surrogate, group)
	if !ok || access != "read" || reveal {
		t.Fatalf("grant = %q,%v,%v; want read,false,true", access, reveal, ok)
	}

	// Re-grant with a stronger level and reveal on → upsert in place.
	if err := svc.GrantSphereByRecordID(ctx, rc, rid, group, data.AccessWrite, true); err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	access, reveal, ok = readGrant(t, conns, rc, surrogate, group)
	if !ok || access != "write" || !reveal {
		t.Fatalf("re-grant = %q,%v,%v; want write,true,true", access, reveal, ok)
	}
}

// TestGrantSphereByRecordIDInvalidAccessRejected asserts a grant with a level
// other than read or write is ErrInvalidAccessLevel and writes no row.
func TestGrantSphereByRecordIDInvalidAccessRejected(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	svc := NewUserGroupService(conns)

	group := seedGroup(t, central, tid, "editors", false)
	rid, surrogate := seedSphereRecordID(t, conns, rc, "Ops")

	if err := svc.GrantSphereByRecordID(context.Background(), rc, rid, group, data.AccessNone, false); !errors.Is(err, ErrInvalidAccessLevel) {
		t.Fatalf("err = %v, want ErrInvalidAccessLevel", err)
	}
	if _, _, ok := readGrant(t, conns, rc, surrogate, group); ok {
		t.Fatalf("an invalid grant wrote a row")
	}
}

// TestGrantSphereByRecordIDCrossTenantGroupRejected asserts a grant naming a
// Group in another tenant is ErrGroupNotAccessible and writes no row.
func TestGrantSphereByRecordIDCrossTenantGroupRejected(t *testing.T) {
	conns, central, _, rc := newUserGroupEnv(t)
	svc := NewUserGroupService(conns)

	// A Group belonging to a different tenant.
	if _, err := central.Exec(
		`INSERT INTO "TENANT"(id, tenant_uuid, name, db_path, status, created_at)
		 VALUES(999, 'uuid-other', 'Other', 'unused', 'active', '2024-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	foreign := seedGroup(t, central, data.TenantID(999), "outsiders", false)
	rid, surrogate := seedSphereRecordID(t, conns, rc, "Ops")

	if err := svc.GrantSphereByRecordID(context.Background(), rc, rid, foreign, data.AccessRead, false); !errors.Is(err, ErrGroupNotAccessible) {
		t.Fatalf("err = %v, want ErrGroupNotAccessible", err)
	}
	if _, _, ok := readGrant(t, conns, rc, surrogate, foreign); ok {
		t.Fatalf("a cross-tenant grant wrote a row")
	}
}

// TestGrantSphereByRecordIDMissingSphereRejected asserts a grant naming a Sphere
// Record_ID absent from the caller's tenant is ErrSphereNotAccessible.
func TestGrantSphereByRecordIDMissingSphereRejected(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	svc := NewUserGroupService(conns)

	group := seedGroup(t, central, tid, "editors", false)
	// A syntactically valid Record_ID that was never inserted.
	missing := recordid.New()

	if err := svc.GrantSphereByRecordID(context.Background(), rc, missing, group, data.AccessRead, false); !errors.Is(err, ErrSphereNotAccessible) {
		t.Fatalf("err = %v, want ErrSphereNotAccessible", err)
	}
}

// TestRevokeSphereByRecordIDRemovesGrant asserts a revoke addressed by the
// Sphere Record_ID removes the grant, and revoking a nonexistent grant is a
// no-op success.
func TestRevokeSphereByRecordIDRemovesGrant(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	ctx := context.Background()
	svc := NewUserGroupService(conns)

	group := seedGroup(t, central, tid, "editors", false)
	rid, surrogate := seedSphereRecordID(t, conns, rc, "Ops")

	if err := svc.GrantSphereByRecordID(ctx, rc, rid, group, data.AccessWrite, true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := svc.RevokeSphereByRecordID(ctx, rc, rid, group); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, ok := readGrant(t, conns, rc, surrogate, group); ok {
		t.Fatalf("revoke left the grant in place")
	}
	// Revoking again is a no-op success.
	if err := svc.RevokeSphereByRecordID(ctx, rc, rid, group); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
}

// TestListSphereGrantsScopedToTenant asserts the listing returns every grant in
// the caller's tenant keyed by the Sphere Record_ID, with access + reveal.
func TestListSphereGrantsScopedToTenant(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	ctx := context.Background()
	svc := NewUserGroupService(conns)

	group := seedGroup(t, central, tid, "editors", false)
	ridA, _ := seedSphereRecordID(t, conns, rc, "Ops")
	ridB, _ := seedSphereRecordID(t, conns, rc, "Docs")

	if err := svc.GrantSphereByRecordID(ctx, rc, ridA, group, data.AccessWrite, true); err != nil {
		t.Fatalf("grant A: %v", err)
	}
	if err := svc.GrantSphereByRecordID(ctx, rc, ridB, group, data.AccessRead, false); err != nil {
		t.Fatalf("grant B: %v", err)
	}

	grants, err := svc.ListSphereGrants(ctx, rc)
	if err != nil {
		t.Fatalf("ListSphereGrants: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("got %d grants, want 2", len(grants))
	}
	byRID := map[string]SphereGrantEntry{}
	for _, g := range grants {
		byRID[g.SphereRecordID] = g
	}
	a, ok := byRID[ridA.Canonical()]
	if !ok || a.Group != group || a.Access != data.AccessWrite || !a.Reveal {
		t.Fatalf("grant A = %+v, want write+reveal for group %d", a, group)
	}
	b, ok := byRID[ridB.Canonical()]
	if !ok || b.Group != group || b.Access != data.AccessRead || b.Reveal {
		t.Fatalf("grant B = %+v, want read/no-reveal for group %d", b, group)
	}
}
