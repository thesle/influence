package api

// Integration tests for the admin per-Sphere Group access matrix routes wired in
// task 27.4 (Req 5.3, 32.4, 32.6). They drive requests through Server.Router()
// and the real middleware chain, exercising:
//
//   - Non-admin denial: every sphere-access route is refused with
//     ADMIN_REQUIRED for a non-admin session (Req 5.5, 32.5).
//   - Grant/list/revoke round-trip scoped to the caller's tenant (Req 5.3).
//   - Invalid access level is a VALIDATION error (Req 4.2).
//   - Cross-tenant Group and out-of-tenant Sphere are rejected with
//     TENANT_ISOLATION (Req 32.6).

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/influence/influence/internal/recordid"
)

// TestAdminSphereAccessRoutesDeniedForNonAdmin asserts the whole sphere-access
// route family is refused with ADMIN_REQUIRED for a non-admin session.
func TestAdminSphereAccessRoutesDeniedForNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		name   string
		method string
		body   any
	}{
		{"list", http.MethodGet, nil},
		{"grant", http.MethodPut, grantSphereAccessRequest{SphereRecordID: e.sphereRecordID, GroupID: 1, Access: "read"}},
		{"revoke", http.MethodDelete, revokeSphereAccessRequest{SphereRecordID: e.sphereRecordID, GroupID: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, tc.method, "/api/admin/sphere-access", e.sessionToken, tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
			}
			if code := decodeEnvelopeCode(t, rec); code != codeAdminRequired {
				t.Errorf("error code = %q, want %q", code, codeAdminRequired)
			}
		})
	}
}

// tenant1EditorsGroupID returns the id of the seeded non-admin "editors" Group
// in tenant 1.
func tenant1EditorsGroupID(t *testing.T, e *testEnv) int64 {
	t.Helper()
	var id int64
	if err := e.central.QueryRow(
		`SELECT id FROM "GROUP" WHERE tenant_id = 1 AND name = 'editors'`,
	).Scan(&id); err != nil {
		t.Fatalf("lookup editors group: %v", err)
	}
	return id
}

// TestGrantListRevokeSphereAccessRoundTrip drives a full grant → list → revoke
// cycle through the admin routes and asserts the listing reflects each step
// (Req 5.3, 32.4).
func TestGrantListRevokeSphereAccessRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	group := tenant1EditorsGroupID(t, e)
	// A fresh Sphere with no seeded grant so the listing is deterministic.
	sphere := insertSphere(t, e.tenant, "Finance")

	// Grant editors write+reveal on Finance.
	grantRec := e.do(t, http.MethodPut, "/api/admin/sphere-access", e.adminToken, grantSphereAccessRequest{
		SphereRecordID: sphere.canonical,
		GroupID:        group,
		Access:         "write",
		Reveal:         true,
	})
	if grantRec.Code != http.StatusOK {
		t.Fatalf("grant status = %d, want 200 (body %q)", grantRec.Code, grantRec.Body.String())
	}

	// The listing shows the new grant.
	list := listSphereGrants(t, e)
	got, ok := list[sphere.canonical]
	if !ok || got.GroupID != group || got.Access != "write" || !got.Reveal {
		t.Fatalf("grant in listing = %+v (ok=%v), want write+reveal for group %d", got, ok, group)
	}

	// Revoke it.
	revokeRec := e.do(t, http.MethodDelete, "/api/admin/sphere-access", e.adminToken, revokeSphereAccessRequest{
		SphereRecordID: sphere.canonical,
		GroupID:        group,
	})
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200 (body %q)", revokeRec.Code, revokeRec.Body.String())
	}
	if _, ok := listSphereGrants(t, e)[sphere.canonical]; ok {
		t.Fatalf("grant still present after revoke")
	}
}

// TestGrantSphereAccessInvalidLevelIsValidation asserts an access level other
// than read or write is a 400 VALIDATION (Req 4.2).
func TestGrantSphereAccessInvalidLevelIsValidation(t *testing.T) {
	e := newTestEnv(t)
	group := tenant1EditorsGroupID(t, e)

	rec := e.do(t, http.MethodPut, "/api/admin/sphere-access", e.adminToken, grantSphereAccessRequest{
		SphereRecordID: e.sphereRecordID,
		GroupID:        group,
		Access:         "owner",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
}

// TestGrantSphereAccessCrossTenantGroupRejected asserts granting to a Group in
// another tenant is rejected with TENANT_ISOLATION (Req 32.6).
func TestGrantSphereAccessCrossTenantGroupRejected(t *testing.T) {
	e := newTestEnv(t)
	foreignGroup, _ := secondTenantFixture(t, e)

	rec := e.do(t, http.MethodPut, "/api/admin/sphere-access", e.adminToken, grantSphereAccessRequest{
		SphereRecordID: e.sphereRecordID,
		GroupID:        int64(foreignGroup),
		Access:         "read",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeTenantIsolation {
		t.Errorf("error code = %q, want %q", code, codeTenantIsolation)
	}
}

// TestGrantSphereAccessMissingSphereRejected asserts granting on a Sphere
// Record_ID absent from the caller's tenant is rejected with TENANT_ISOLATION
// (Req 32.6): an out-of-tenant Sphere is indistinguishable from a nonexistent
// one.
func TestGrantSphereAccessMissingSphereRejected(t *testing.T) {
	e := newTestEnv(t)
	group := tenant1EditorsGroupID(t, e)

	rec := e.do(t, http.MethodPut, "/api/admin/sphere-access", e.adminToken, grantSphereAccessRequest{
		SphereRecordID: recordid.New().Canonical(),
		GroupID:        group,
		Access:         "read",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeTenantIsolation {
		t.Errorf("error code = %q, want %q", code, codeTenantIsolation)
	}
}

// TestGrantSphereAccessInvalidSphereIDIsValidation asserts a malformed Sphere
// identifier is a 400 VALIDATION before reaching the service.
func TestGrantSphereAccessInvalidSphereIDIsValidation(t *testing.T) {
	e := newTestEnv(t)
	group := tenant1EditorsGroupID(t, e)

	rec := e.do(t, http.MethodPut, "/api/admin/sphere-access", e.adminToken, grantSphereAccessRequest{
		SphereRecordID: "not-a-record-id",
		GroupID:        group,
		Access:         "read",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
}

// TestListSphereAccessScopedToCallerTenant asserts the listing returns only the
// caller's tenant's grants (the seeded Ops grant), keyed by Record_ID.
func TestListSphereAccessScopedToCallerTenant(t *testing.T) {
	e := newTestEnv(t)

	list := listSphereGrants(t, e)
	// The env seeds a write+reveal grant of "editors" on the "Ops" sphere.
	got, ok := list[e.sphereRecordID]
	if !ok {
		t.Fatalf("seeded Ops grant missing from listing: %+v", list)
	}
	if got.Access != "write" || !got.Reveal {
		t.Fatalf("seeded grant = %+v, want write+reveal", got)
	}
	if got.GroupID != tenant1EditorsGroupID(t, e) {
		t.Fatalf("seeded grant group = %d, want editors group", got.GroupID)
	}
}

// listSphereGrants performs GET /api/admin/sphere-access as the admin and
// returns the grants keyed by Sphere Record_ID.
func listSphereGrants(t *testing.T, e *testEnv) map[string]sphereGrantResponse {
	t.Helper()
	rec := e.do(t, http.MethodGet, "/api/admin/sphere-access", e.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Grants []sphereGrantResponse `json:"grants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode grants: %v", err)
	}
	out := make(map[string]sphereGrantResponse, len(body.Grants))
	for _, g := range body.Grants {
		out[g.SphereRecordID] = g
	}
	return out
}
