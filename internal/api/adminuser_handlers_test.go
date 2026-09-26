package api

// Integration tests for the admin user-lifecycle routes wired in task 27.2
// (Req 32.2, 5.1, 5.4, 32.6). They drive requests through Server.Router() and
// the real middleware chain, exercising:
//
//   - Non-admin denial: every admin user route is refused with ADMIN_REQUIRED
//     for a non-admin session (Req 5.5, 32.5).
//   - Create round-trip: POST creates a user in the caller's tenant and adds it
//     to the requested Group; a weak password is VALIDATION (Req 32.2).
//   - Transactional rollback: a create naming an out-of-tenant Group is rejected
//     with TENANT_ISOLATION and leaves no USER row (Req 32.6, 32.7).
//   - List scoping: the listing returns only the caller's tenant's users
//     (Req 32.6).
//   - Deactivate/reactivate toggle the flag (Req 5.4) and reject a cross-tenant
//     user with TENANT_ISOLATION (Req 32.6).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/influence/influence/internal/data"
)

// tenant1GroupID returns the id of the seeded non-admin "editors" Group in
// tenant 1, which is a valid initial Group for create tests.
func tenant1GroupID(t *testing.T, e *testEnv) int64 {
	t.Helper()
	var id int64
	if err := e.central.QueryRow(
		`SELECT id FROM "GROUP" WHERE tenant_id = 1 AND name = 'editors'`,
	).Scan(&id); err != nil {
		t.Fatalf("lookup editors group: %v", err)
	}
	return id
}

// secondTenantFixture seeds a second tenant (id 2) with one Group and one User
// directly in the Central_Directory, returning their ids so cross-tenant
// isolation can be exercised. The tenant needs no database for these tests.
func secondTenantFixture(t *testing.T, e *testEnv) (groupID data.GroupID, userID data.UserID) {
	t.Helper()
	if _, err := e.central.Exec(
		`INSERT INTO "TENANT" (id, tenant_uuid, name, db_path, status, created_at)
		 VALUES (2, 'tenant-b-uuid', 'B', 'unused', 'active', '2024-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	gid := insertGroup(t, e.central, 2, "outsiders", false)
	uid := insertUser(t, e.central, 2, "eve", "Eve")
	insertMembership(t, e.central, uid, gid)
	return gid, uid
}

// TestAdminUserRoutesDeniedForNonAdmin asserts the whole admin user route family
// is refused with ADMIN_REQUIRED for a non-admin session and mutates nothing.
func TestAdminUserRoutesDeniedForNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"list users", http.MethodGet, "/api/admin/users", nil},
		{"create user", http.MethodPost, "/api/admin/users", createUserRequest{Username: "x", Password: "Sup3rSecret!pw", GroupID: 1}},
		{"deactivate user", http.MethodPost, "/api/admin/users/1/deactivate", nil},
		{"reactivate user", http.MethodPost, "/api/admin/users/1/reactivate", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, tc.method, tc.target, e.sessionToken, tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
			}
			if code := decodeEnvelopeCode(t, rec); code != codeAdminRequired {
				t.Errorf("error code = %q, want %q", code, codeAdminRequired)
			}
		})
	}
}

// TestCreateUserRoundTrip verifies POST /api/admin/users creates a user in the
// caller's tenant, adds the initial membership, and lists back afterward.
func TestCreateUserRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	gid := tenant1GroupID(t, e)

	rec := e.do(t, http.MethodPost, "/api/admin/users", e.adminToken, createUserRequest{
		Username:    "newbie",
		DisplayName: "New Bie",
		Password:    "Sup3rSecret!pw",
		GroupID:     gid,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var created adminUserResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == 0 || created.Username != "newbie" || created.DisplayName != "New Bie" || created.Deactivated {
		t.Fatalf("created = %+v, want id set, newbie/New Bie, active", created)
	}

	// The initial membership exists.
	var members int
	if err := e.central.QueryRow(
		`SELECT COUNT(*) FROM "GROUP_MEMBERSHIP" WHERE user_id = ? AND group_id = ?`,
		created.ID, gid,
	).Scan(&members); err != nil {
		t.Fatalf("count membership: %v", err)
	}
	if members != 1 {
		t.Fatalf("membership count = %d, want 1", members)
	}

	// The new user appears in the listing.
	listRec := e.do(t, http.MethodGet, "/api/admin/users", e.adminToken, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %q)", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Users []adminUserResponse `json:"users"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, u := range list.Users {
		if u.Username == "newbie" {
			found = true
		}
	}
	if !found {
		t.Fatalf("created user not in listing: %+v", list.Users)
	}
}

// TestCreateUserWeakPasswordIsValidation asserts a weak password is a 400
// VALIDATION and no user is created.
func TestCreateUserWeakPasswordIsValidation(t *testing.T) {
	e := newTestEnv(t)
	gid := tenant1GroupID(t, e)

	rec := e.do(t, http.MethodPost, "/api/admin/users", e.adminToken, createUserRequest{
		Username: "weak",
		Password: "short",
		GroupID:  gid,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
	var n int
	if err := e.central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE username = 'weak'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("weak-password create persisted %d users, want 0", n)
	}
}

// TestCreateUserCrossTenantGroupRejected asserts a create naming a Group in
// another tenant is rejected with TENANT_ISOLATION and leaves no USER row,
// proving the transactional rollback of Req 32.7 through the wired route.
func TestCreateUserCrossTenantGroupRejected(t *testing.T) {
	e := newTestEnv(t)
	foreignGroup, _ := secondTenantFixture(t, e)

	rec := e.do(t, http.MethodPost, "/api/admin/users", e.adminToken, createUserRequest{
		Username: "mallory",
		Password: "Sup3rSecret!pw",
		GroupID:  int64(foreignGroup),
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeTenantIsolation {
		t.Errorf("error code = %q, want %q", code, codeTenantIsolation)
	}
	var n int
	if err := e.central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE username = 'mallory'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("cross-tenant create persisted %d users, want 0 (rollback)", n)
	}
}

// TestListUsersScopedToCallerTenant asserts the listing returns only tenant 1's
// users, never the second tenant's.
func TestListUsersScopedToCallerTenant(t *testing.T) {
	e := newTestEnv(t)
	secondTenantFixture(t, e) // seeds user "eve" in tenant 2

	rec := e.do(t, http.MethodGet, "/api/admin/users", e.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var list struct {
		Users []adminUserResponse `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, u := range list.Users {
		if u.Username == "eve" {
			t.Fatalf("listing leaked a user from another tenant: %+v", list.Users)
		}
	}
	// Tenant 1 has the seeded "alice" and "root" users.
	if len(list.Users) != 2 {
		t.Fatalf("listed %d users, want 2 (alice, root)", len(list.Users))
	}
}

// TestDeactivateReactivateRoundTrip asserts the toggle routes flip
// USER.deactivated for a user in the caller's tenant (Req 5.4).
func TestDeactivateReactivateRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	// Create a target user in tenant 1 so we do not disable the admin itself.
	gid := tenant1GroupID(t, e)
	uid := insertUser(t, e.central, 1, "target", "Target")
	insertMembership(t, e.central, uid, data.GroupID(gid))

	deacRec := e.do(t, http.MethodPost, fmt.Sprintf("/api/admin/users/%d/deactivate", int64(uid)), e.adminToken, nil)
	if deacRec.Code != http.StatusOK {
		t.Fatalf("deactivate status = %d, want 200 (body %q)", deacRec.Code, deacRec.Body.String())
	}
	if !apiReadDeactivated(t, e, int64(uid)) {
		t.Fatal("user should be deactivated after deactivate route")
	}

	reacRec := e.do(t, http.MethodPost, fmt.Sprintf("/api/admin/users/%d/reactivate", int64(uid)), e.adminToken, nil)
	if reacRec.Code != http.StatusOK {
		t.Fatalf("reactivate status = %d, want 200 (body %q)", reacRec.Code, reacRec.Body.String())
	}
	if apiReadDeactivated(t, e, int64(uid)) {
		t.Fatal("user should be active after reactivate route")
	}
}

// TestDeactivateCrossTenantRejected asserts deactivating a user in another
// tenant is rejected with TENANT_ISOLATION and leaves the target untouched.
func TestDeactivateCrossTenantRejected(t *testing.T) {
	e := newTestEnv(t)
	_, foreignUser := secondTenantFixture(t, e)

	rec := e.do(t, http.MethodPost, fmt.Sprintf("/api/admin/users/%d/deactivate", int64(foreignUser)), e.adminToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeTenantIsolation {
		t.Errorf("error code = %q, want %q", code, codeTenantIsolation)
	}
	if apiReadDeactivated(t, e, int64(foreignUser)) {
		t.Fatal("cross-tenant user must be left untouched")
	}
}

// TestDeactivateInvalidIDIsValidation asserts a non-numeric {id} is a 400
// VALIDATION rather than reaching the service.
func TestDeactivateInvalidIDIsValidation(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodPost, "/api/admin/users/not-a-number/deactivate", e.adminToken, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
}

// apiReadDeactivated reads the deactivated flag for a user directly from central.
func apiReadDeactivated(t *testing.T, e *testEnv, userID int64) bool {
	t.Helper()
	var d int
	if err := e.central.QueryRow(`SELECT deactivated FROM "USER" WHERE id = ?`, userID).Scan(&d); err != nil {
		t.Fatalf("read deactivated: %v", err)
	}
	return d != 0
}
