package api

// Integration tests for the admin Group + membership routes wired in task 27.3
// (Req 5.2, 32.3, 4.1, 32.6). They drive requests through Server.Router() and
// the real middleware chain, exercising:
//
//   - Non-admin denial: every admin group route is refused with ADMIN_REQUIRED
//     for a non-admin session (Req 5.5, 32.5).
//   - List/create scoping: the listing returns only the caller's tenant's
//     Groups; create persists a Group in the caller's tenant (Req 5.2, 32.6).
//   - Add/remove membership round-trip (Req 32.3).
//   - Min-one-Group invariant: a removal that would leave a user with zero
//     Groups is a VALIDATION error and leaves the membership intact (Req 4.1).
//   - Cross-tenant isolation: a Group or User in another tenant is rejected
//     with TENANT_ISOLATION (Req 32.6).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/influence/influence/internal/data"
)

// TestAdminGroupRoutesDeniedForNonAdmin asserts the whole admin group route
// family is refused with ADMIN_REQUIRED for a non-admin session.
func TestAdminGroupRoutesDeniedForNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"list groups", http.MethodGet, "/api/admin/groups", nil},
		{"create group", http.MethodPost, "/api/admin/groups", createGroupRequest{Name: "x"}},
		{"add member", http.MethodPost, "/api/admin/groups/1/members", groupMemberRequest{UserID: 1}},
		{"remove member", http.MethodDelete, "/api/admin/groups/1/members/1", nil},
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

// TestListGroupsScopedToCallerTenant asserts the listing returns only tenant 1's
// Groups (the seeded "editors" and "admins"), never another tenant's.
func TestListGroupsScopedToCallerTenant(t *testing.T) {
	e := newTestEnv(t)
	secondTenantFixture(t, e) // seeds Group "outsiders" in tenant 2

	rec := e.do(t, http.MethodGet, "/api/admin/groups", e.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var list struct {
		Groups []adminGroupResponse `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	names := map[string]bool{}
	for _, g := range list.Groups {
		if g.Name == "outsiders" {
			t.Fatalf("listing leaked a group from another tenant: %+v", list.Groups)
		}
		names[g.Name] = true
	}
	if !names["editors"] || !names["admins"] {
		t.Fatalf("listing missing seeded groups: %+v", list.Groups)
	}
	if len(list.Groups) != 2 {
		t.Fatalf("listed %d groups, want 2 (editors, admins)", len(list.Groups))
	}
}

// TestCreateGroupRoundTrip verifies POST /api/admin/groups creates a Group in
// the caller's tenant and it appears in the subsequent listing.
func TestCreateGroupRoundTrip(t *testing.T) {
	e := newTestEnv(t)

	rec := e.do(t, http.MethodPost, "/api/admin/groups", e.adminToken, createGroupRequest{Name: "reviewers"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var created adminGroupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == 0 || created.Name != "reviewers" || created.IsAdmin {
		t.Fatalf("created = %+v, want id set, reviewers, non-admin", created)
	}

	var n int
	if err := e.central.QueryRow(
		`SELECT COUNT(*) FROM "GROUP" WHERE tenant_id = 1 AND name = 'reviewers' AND is_admin = 0`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("persisted %d groups, want 1", n)
	}
}

// TestCreateGroupEmptyNameIsValidation asserts an empty name is a 400 VALIDATION.
func TestCreateGroupEmptyNameIsValidation(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodPost, "/api/admin/groups", e.adminToken, createGroupRequest{Name: ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
}

// TestAddRemoveMemberRoundTrip verifies a user can be added to a second Group
// and then removed while retaining their original Group (Req 32.3).
func TestAddRemoveMemberRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	// A target user starts in the "editors" Group so removing the new Group
	// never trips the min-one-Group invariant.
	editors := tenant1GroupID(t, e)
	uid := insertUser(t, e.central, 1, "target", "Target")
	insertMembership(t, e.central, uid, data.GroupID(editors))

	// Create a second Group and add the user to it.
	created := e.do(t, http.MethodPost, "/api/admin/groups", e.adminToken, createGroupRequest{Name: "reviewers"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create group status = %d (body %q)", created.Code, created.Body.String())
	}
	var group adminGroupResponse
	if err := json.Unmarshal(created.Body.Bytes(), &group); err != nil {
		t.Fatalf("decode group: %v", err)
	}

	addRec := e.do(t, http.MethodPost, fmt.Sprintf("/api/admin/groups/%d/members", group.ID), e.adminToken, groupMemberRequest{UserID: int64(uid)})
	if addRec.Code != http.StatusOK {
		t.Fatalf("add member status = %d (body %q)", addRec.Code, addRec.Body.String())
	}
	if membershipCountAPI(t, e, int64(uid)) != 2 {
		t.Fatalf("membership count = %d, want 2 after add", membershipCountAPI(t, e, int64(uid)))
	}

	rmRec := e.do(t, http.MethodDelete, fmt.Sprintf("/api/admin/groups/%d/members/%d", group.ID, int64(uid)), e.adminToken, nil)
	if rmRec.Code != http.StatusOK {
		t.Fatalf("remove member status = %d (body %q)", rmRec.Code, rmRec.Body.String())
	}
	if membershipCountAPI(t, e, int64(uid)) != 1 {
		t.Fatalf("membership count = %d, want 1 after remove", membershipCountAPI(t, e, int64(uid)))
	}
}

// TestRemoveLastMembershipIsValidation asserts removing a user's only Group is a
// 400 VALIDATION and leaves the membership intact (Req 4.1).
func TestRemoveLastMembershipIsValidation(t *testing.T) {
	e := newTestEnv(t)
	editors := tenant1GroupID(t, e)
	uid := insertUser(t, e.central, 1, "solo", "Solo")
	insertMembership(t, e.central, uid, data.GroupID(editors))

	rec := e.do(t, http.MethodDelete, fmt.Sprintf("/api/admin/groups/%d/members/%d", editors, int64(uid)), e.adminToken, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
	if membershipCountAPI(t, e, int64(uid)) != 1 {
		t.Fatalf("min-group removal changed membership count to %d, want 1", membershipCountAPI(t, e, int64(uid)))
	}
}

// TestAddMemberCrossTenantGroupRejected asserts adding to a Group in another
// tenant is rejected with TENANT_ISOLATION (Req 32.6).
func TestAddMemberCrossTenantGroupRejected(t *testing.T) {
	e := newTestEnv(t)
	foreignGroup, _ := secondTenantFixture(t, e)
	uid := insertUser(t, e.central, 1, "local", "Local")

	rec := e.do(t, http.MethodPost, fmt.Sprintf("/api/admin/groups/%d/members", int64(foreignGroup)), e.adminToken, groupMemberRequest{UserID: int64(uid)})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeTenantIsolation {
		t.Errorf("error code = %q, want %q", code, codeTenantIsolation)
	}
}

// TestAddMemberCrossTenantUserRejected asserts adding a User from another tenant
// to a local Group is rejected with TENANT_ISOLATION (Req 32.6).
func TestAddMemberCrossTenantUserRejected(t *testing.T) {
	e := newTestEnv(t)
	_, foreignUser := secondTenantFixture(t, e)
	localGroup := tenant1GroupID(t, e)

	rec := e.do(t, http.MethodPost, fmt.Sprintf("/api/admin/groups/%d/members", localGroup), e.adminToken, groupMemberRequest{UserID: int64(foreignUser)})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeTenantIsolation {
		t.Errorf("error code = %q, want %q", code, codeTenantIsolation)
	}
}

// TestAddMemberInvalidGroupIDIsValidation asserts a non-numeric {id} is a 400
// VALIDATION before reaching the service.
func TestAddMemberInvalidGroupIDIsValidation(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodPost, "/api/admin/groups/not-a-number/members", e.adminToken, groupMemberRequest{UserID: 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
}

// membershipCountAPI reads how many Groups a user belongs to directly from
// central.
func membershipCountAPI(t *testing.T, e *testEnv, userID int64) int {
	t.Helper()
	var n int
	if err := e.central.QueryRow(
		`SELECT COUNT(*) FROM "GROUP_MEMBERSHIP" WHERE user_id = ?`, userID,
	).Scan(&n); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	return n
}
