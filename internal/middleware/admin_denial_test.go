package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/service"
)

// This file adds the example-level denial assertions task 6.4 calls for on top
// of the coverage already in chain_test.go. Non-admin admin-route denial
// (Req 5.5) and deactivated-admin denial (Req 5.4) are covered there by
// TestRequireAdmin_DeniesNonAdmin and TestRequireAdmin_DeactivatedAdminDenied.
// The gap those leave is a DEACTIVATED non-admin user who still carries a Sphere
// grant: Req 5.4 says such a user must be denied BOTH Sphere access and the
// admin pages even though the grant row is present and would otherwise let them
// in. The tests below drive that case through the middleware guards.

// TestDeactivatedUser_DeniedSphereAccess asserts that a non-admin whose account
// is deactivated is refused a Sphere route with 403 NO_SPHERE_ACCESS even
// though a read grant for that exact Sphere is present in AuthzData. The
// deactivated override in the PolicyEngine collapses the grant to AccessNone,
// so RequireSphereAccess denies and the handler never runs (Req 5.4).
func TestDeactivatedUser_DeniedSphereAccess(t *testing.T) {
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{UserID: 1, TenantID: 1}},
		fakeLoader{
			groups: []data.GroupID{10},
			authz: service.AuthzData{
				Deactivated: true,
				Grants: []service.GroupGrant{
					{Group: 10, Sphere: 5, Access: data.AccessRead},
				},
			},
		},
	)

	ran := false
	r := chi.NewRouter()
	r.Use(chain.Middleware())
	r.With(chain.RequireSphereAccess("sphereID", data.AccessRead)).
		Get("/spheres/{sphereID}", func(w http.ResponseWriter, req *http.Request) {
			ran = true
			w.WriteHeader(http.StatusOK)
		})

	req := httptest.NewRequest(http.MethodGet, "/spheres/5", nil)
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := decodeError(t, rec.Body.Bytes()).Code; code != "NO_SPHERE_ACCESS" {
		t.Errorf("error code = %q, want NO_SPHERE_ACCESS", code)
	}
	if ran {
		t.Error("deactivated user reached the Sphere handler despite a present grant")
	}
}

// TestDeactivatedNonAdmin_DeniedAdminPages asserts that a deactivated non-admin
// is refused an admin route with 403 ADMIN_REQUIRED and the "admin privileges
// required" message. This complements TestRequireAdmin_DeactivatedAdminDenied
// (a deactivated *admin*) by covering a deactivated user who was never an admin,
// closing out the "denied both Spheres and admin pages" half of Req 5.4 for the
// non-admin case.
func TestDeactivatedNonAdmin_DeniedAdminPages(t *testing.T) {
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{UserID: 1, TenantID: 1}},
		fakeLoader{
			groups: []data.GroupID{10},
			authz:  service.AuthzData{Admin: false, Deactivated: true},
		},
	)

	adminRan := false
	admin := chain.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminRan = true
	}))
	h := chain.Handler(admin)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	got := decodeError(t, rec.Body.Bytes())
	if got.Code != "ADMIN_REQUIRED" {
		t.Errorf("error code = %q, want ADMIN_REQUIRED", got.Code)
	}
	if got.Message != "admin privileges required" {
		t.Errorf("error message = %q, want %q", got.Message, "admin privileges required")
	}
	if adminRan {
		t.Error("deactivated non-admin reached the admin handler")
	}
}
