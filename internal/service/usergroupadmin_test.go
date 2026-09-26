package service

// Unit tests for the admin Group half of Requirement 5.2 / 32.3: ListGroups,
// CreateGroup, and the tenant-scoped membership wrappers. They exercise the
// service directly over a real ConnManager (newUserGroupEnv) so the tenant
// scoping and the min-one-Group invariant are proven at the service boundary,
// independent of the REST wiring.

import (
	"context"
	"errors"
	"testing"

	"github.com/influence/influence/internal/data"
)

// TestListGroupsScopedToTenant asserts ListGroups returns only the caller's
// tenant's Groups, ordered by name, never a Group from another tenant.
func TestListGroupsScopedToTenant(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	ctx := context.Background()

	seedGroup(t, central, tid, "editors", false)
	seedGroup(t, central, tid, "admins", true)

	// A second tenant with its own Group that must never appear.
	if _, err := central.Exec(
		`INSERT INTO "TENANT"(id, tenant_uuid, name, db_path, status, created_at)
		 VALUES(999, 'uuid-other', 'Other', 'unused', 'active', '2024-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	seedGroup(t, central, data.TenantID(999), "outsiders", false)

	groups, err := (NewUserGroupService(conns)).ListGroups(ctx, rc)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (admins, editors)", len(groups))
	}
	// Ordered by name.
	if groups[0].Name != "admins" || groups[1].Name != "editors" {
		t.Fatalf("group order = %q,%q; want admins,editors", groups[0].Name, groups[1].Name)
	}
	if !groups[0].IsAdmin || groups[1].IsAdmin {
		t.Fatalf("is_admin flags wrong: %+v", groups)
	}
	for _, g := range groups {
		if g.Name == "outsiders" {
			t.Fatalf("listing leaked a group from another tenant: %+v", groups)
		}
	}
}

// TestCreateGroupRoundTrip asserts CreateGroup persists a non-admin Group in the
// caller's tenant and returns its id.
func TestCreateGroupRoundTrip(t *testing.T) {
	conns, central, _, rc := newUserGroupEnv(t)
	ctx := context.Background()
	svc := NewUserGroupService(conns)

	g, err := svc.CreateGroup(ctx, rc, "reviewers")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if g.ID == 0 || g.Name != "reviewers" || g.IsAdmin {
		t.Fatalf("created = %+v, want id set, reviewers, non-admin", g)
	}
	var n int
	if err := central.QueryRow(
		`SELECT COUNT(*) FROM "GROUP" WHERE id = ? AND name = 'reviewers' AND is_admin = 0`, g.ID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("persisted %d rows, want 1", n)
	}
}

// TestCreateGroupEmptyNameRejected asserts an empty name is ErrGroupNameRequired.
func TestCreateGroupEmptyNameRejected(t *testing.T) {
	conns, _, _, rc := newUserGroupEnv(t)
	svc := NewUserGroupService(conns)
	if _, err := svc.CreateGroup(context.Background(), rc, ""); !errors.Is(err, ErrGroupNameRequired) {
		t.Fatalf("err = %v, want ErrGroupNameRequired", err)
	}
}

// TestCreateGroupDuplicateNameRejected asserts a duplicate name within the
// tenant is ErrGroupNameTaken and no second row is created.
func TestCreateGroupDuplicateNameRejected(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	seedGroup(t, central, tid, "editors", false)
	svc := NewUserGroupService(conns)
	if _, err := svc.CreateGroup(context.Background(), rc, "editors"); !errors.Is(err, ErrGroupNameTaken) {
		t.Fatalf("err = %v, want ErrGroupNameTaken", err)
	}
	var n int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "GROUP" WHERE tenant_id = ? AND name = 'editors'`, int64(tid)).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("group count = %d, want 1 (no duplicate)", n)
	}
}

// TestAddMembershipScopedRoundTrip asserts a scoped add creates the membership
// when both the User and Group are in the caller's tenant.
func TestAddMembershipScopedRoundTrip(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	svc := NewUserGroupService(conns)

	g := seedGroup(t, central, tid, "editors", false)
	uid := data.UserID(seedUser(t, central, int64(tid), "alice", "hunter2"))

	if err := svc.AddMembershipScoped(context.Background(), rc, uid, g); err != nil {
		t.Fatalf("AddMembershipScoped: %v", err)
	}
	if membershipCount(t, central, uid) != 1 {
		t.Fatalf("membership count = %d, want 1", membershipCount(t, central, uid))
	}
}

// TestAddMembershipScopedCrossTenantGroupRejected asserts adding to a Group in
// another tenant is ErrGroupNotAccessible and creates no membership.
func TestAddMembershipScopedCrossTenantGroupRejected(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	svc := NewUserGroupService(conns)

	uid := data.UserID(seedUser(t, central, int64(tid), "alice", "hunter2"))
	// A Group owned by a different tenant.
	if _, err := central.Exec(
		`INSERT INTO "TENANT"(id, tenant_uuid, name, db_path, status, created_at)
		 VALUES(999, 'uuid-other', 'Other', 'unused', 'active', '2024-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	foreign := seedGroup(t, central, data.TenantID(999), "outsiders", false)

	if err := svc.AddMembershipScoped(context.Background(), rc, uid, foreign); !errors.Is(err, ErrGroupNotAccessible) {
		t.Fatalf("err = %v, want ErrGroupNotAccessible", err)
	}
	if membershipCount(t, central, uid) != 0 {
		t.Fatalf("cross-tenant add created a membership")
	}
}

// TestAddMembershipScopedCrossTenantUserRejected asserts adding a User from
// another tenant to a local Group is ErrUserNotAccessible.
func TestAddMembershipScopedCrossTenantUserRejected(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	svc := NewUserGroupService(conns)

	local := seedGroup(t, central, tid, "editors", false)
	if _, err := central.Exec(
		`INSERT INTO "TENANT"(id, tenant_uuid, name, db_path, status, created_at)
		 VALUES(999, 'uuid-other', 'Other', 'unused', 'active', '2024-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	foreignUser := data.UserID(seedUser(t, central, 999, "eve", "hunter2"))

	if err := svc.AddMembershipScoped(context.Background(), rc, foreignUser, local); !errors.Is(err, ErrUserNotAccessible) {
		t.Fatalf("err = %v, want ErrUserNotAccessible", err)
	}
}

// TestRemoveMembershipScopedLastGroupIsMinGroup asserts removing a User's last
// Group is ErrMinGroupMembership and leaves the membership intact (Req 4.1).
func TestRemoveMembershipScopedLastGroupIsMinGroup(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	svc := NewUserGroupService(conns)

	g := seedGroup(t, central, tid, "editors", false)
	uid := data.UserID(seedUser(t, central, int64(tid), "alice", "hunter2"))
	if err := svc.AddMembership(context.Background(), uid, g); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	if err := svc.RemoveMembershipScoped(context.Background(), rc, uid, g); !errors.Is(err, ErrMinGroupMembership) {
		t.Fatalf("err = %v, want ErrMinGroupMembership", err)
	}
	if membershipCount(t, central, uid) != 1 {
		t.Fatalf("last-group removal changed membership count to %d, want 1", membershipCount(t, central, uid))
	}
}

// TestRemoveMembershipScopedNonLastSucceeds asserts a scoped removal succeeds
// when the User retains at least one other Group.
func TestRemoveMembershipScopedNonLastSucceeds(t *testing.T) {
	conns, central, tid, rc := newUserGroupEnv(t)
	svc := NewUserGroupService(conns)

	g1 := seedGroup(t, central, tid, "editors", false)
	g2 := seedGroup(t, central, tid, "reviewers", false)
	uid := data.UserID(seedUser(t, central, int64(tid), "alice", "hunter2"))
	if err := svc.AddMembership(context.Background(), uid, g1); err != nil {
		t.Fatalf("seed membership g1: %v", err)
	}
	if err := svc.AddMembership(context.Background(), uid, g2); err != nil {
		t.Fatalf("seed membership g2: %v", err)
	}

	if err := svc.RemoveMembershipScoped(context.Background(), rc, uid, g1); err != nil {
		t.Fatalf("RemoveMembershipScoped: %v", err)
	}
	if membershipCount(t, central, uid) != 1 {
		t.Fatalf("membership count = %d, want 1 after removing one of two", membershipCount(t, central, uid))
	}
}
