package api

// Consolidated cross-family example tests for the two authorization guarantees
// every admin route must uphold (task 27.6), driven through Server.Router() and
// the real middleware chain:
//
//   - Admin-only access (Req 5.5, 32.5): a non-admin caller is refused with
//     ADMIN_REQUIRED before any handler runs, and the request mutates nothing.
//   - Tenant isolation (Req 32.6): a request naming an entity outside the
//     caller's tenant is rejected with TENANT_ISOLATION and leaves state
//     unchanged.
//
// These are example/table-driven tests spanning EACH admin route family
// (users, groups, sphere-access) in one place; per-family behavioural detail
// lives in adminuser_handlers_test.go, admingroup_handlers_test.go and
// spheregrant_handlers_test.go.

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/influence/influence/internal/data"
)

// adminMutationSnapshot captures the row counts an admin mutation could touch,
// so a rejected request can be shown to have changed nothing.
type adminMutationSnapshot struct {
	users       int
	groups      int
	memberships int
	grants      int
}

// snapshotAdminState reads the counts of every table the admin routes can
// mutate, across all tenants (a leak into another tenant would still change a
// count somewhere).
func snapshotAdminState(t *testing.T, e *testEnv) adminMutationSnapshot {
	t.Helper()
	var s adminMutationSnapshot
	count := func(db *dataDB, query string) int {
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil {
			t.Fatalf("count (%s): %v", query, err)
		}
		return n
	}
	s.users = count(e.central, `SELECT COUNT(*) FROM "USER"`)
	s.groups = count(e.central, `SELECT COUNT(*) FROM "GROUP"`)
	s.memberships = count(e.central, `SELECT COUNT(*) FROM "GROUP_MEMBERSHIP"`)
	s.grants = count(e.tenant, `SELECT COUNT(*) FROM sphere_grant`)
	return s
}

// adminRoute names one route in an admin family for the table-driven cases.
type adminRoute struct {
	name   string
	method string
	target string
	body   any
}

// TestAdminDenialAllFamilies asserts that a non-admin session is refused with
// ADMIN_REQUIRED on every route of every admin family, and that the rejected
// mutating requests change nothing (Req 5.5, 32.5).
func TestAdminDenialAllFamilies(t *testing.T) {
	e := newTestEnv(t)

	// Every route across all three families, reads and writes alike.
	routes := []adminRoute{
		{"users: list", http.MethodGet, "/api/admin/users", nil},
		{"users: create", http.MethodPost, "/api/admin/users", createUserRequest{Username: "intruder", Password: "Sup3rSecret!pw", GroupID: 1}},
		{"users: deactivate", http.MethodPost, "/api/admin/users/1/deactivate", nil},
		{"users: reactivate", http.MethodPost, "/api/admin/users/1/reactivate", nil},
		{"groups: list", http.MethodGet, "/api/admin/groups", nil},
		{"groups: create", http.MethodPost, "/api/admin/groups", createGroupRequest{Name: "intruders"}},
		{"groups: add member", http.MethodPost, "/api/admin/groups/1/members", groupMemberRequest{UserID: 1}},
		{"groups: remove member", http.MethodDelete, "/api/admin/groups/1/members/1", nil},
		{"sphere-access: list", http.MethodGet, "/api/admin/sphere-access", nil},
		{"sphere-access: grant", http.MethodPut, "/api/admin/sphere-access", grantSphereAccessRequest{SphereRecordID: e.sphereRecordID, GroupID: 1, Access: "read"}},
		{"sphere-access: revoke", http.MethodDelete, "/api/admin/sphere-access", revokeSphereAccessRequest{SphereRecordID: e.sphereRecordID, GroupID: 1}},
	}

	before := snapshotAdminState(t, e)
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			rec := e.do(t, r.method, r.target, e.sessionToken, r.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
			}
			if code := decodeEnvelopeCode(t, rec); code != codeAdminRequired {
				t.Errorf("error code = %q, want %q", code, codeAdminRequired)
			}
		})
	}
	if after := snapshotAdminState(t, e); after != before {
		t.Fatalf("non-admin requests mutated state: before %+v, after %+v", before, after)
	}
}

// TestAdminCrossTenantAllFamilies asserts that, for each admin family, an admin
// request naming an entity in another tenant is rejected with TENANT_ISOLATION
// and leaves state unchanged (Req 32.6). An out-of-tenant entity is
// indistinguishable from a nonexistent one, so the guard reports 404.
func TestAdminCrossTenantAllFamilies(t *testing.T) {
	e := newTestEnv(t)
	foreignGroup, foreignUser := secondTenantFixture(t, e)
	localGroup := tenant1GroupID(t, e)
	// A local user with a home Group, so a cross-tenant add would be the only
	// change if isolation failed to hold.
	localUser := insertUser(t, e.central, 1, "homebody", "Home Body")
	insertMembership(t, e.central, localUser, data.GroupID(localGroup))

	cases := []adminRoute{
		{
			"users: create into foreign group",
			http.MethodPost, "/api/admin/users",
			createUserRequest{Username: "mallory", Password: "Sup3rSecret!pw", GroupID: int64(foreignGroup)},
		},
		{
			"users: deactivate foreign user",
			http.MethodPost, "/api/admin/users/" + itoa(int64(foreignUser)) + "/deactivate",
			nil,
		},
		{
			"groups: add foreign group",
			http.MethodPost, "/api/admin/groups/" + itoa(int64(foreignGroup)) + "/members",
			groupMemberRequest{UserID: int64(localUser)},
		},
		{
			"groups: add foreign user",
			http.MethodPost, "/api/admin/groups/" + itoa(int64(localGroup)) + "/members",
			groupMemberRequest{UserID: int64(foreignUser)},
		},
		{
			"sphere-access: grant to foreign group",
			http.MethodPut, "/api/admin/sphere-access",
			grantSphereAccessRequest{SphereRecordID: e.sphereRecordID, GroupID: int64(foreignGroup), Access: "read"},
		},
	}

	before := snapshotAdminState(t, e)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.do(t, c.method, c.target, e.adminToken, c.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
			}
			if code := decodeEnvelopeCode(t, rec); code != codeTenantIsolation {
				t.Errorf("error code = %q, want %q", code, codeTenantIsolation)
			}
		})
	}
	if after := snapshotAdminState(t, e); after != before {
		t.Fatalf("cross-tenant requests mutated state: before %+v, after %+v", before, after)
	}
}

// itoa formats an int64 for embedding in a route path, keeping the case table
// readable.
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
