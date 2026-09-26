package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/service"
)

// fakeSessions is a SessionValidator stub: it returns a fixed session for a
// known token and ErrSessionInvalid for anything else, letting the chain tests
// drive the Session stage without a real auth service or database.
type fakeSessions struct {
	validToken string
	session    service.Session
}

func (f fakeSessions) ValidateSession(_ context.Context, token string) (service.Session, error) {
	if token != "" && token == f.validToken {
		return f.session, nil
	}
	return service.Session{}, service.ErrSessionInvalid
}

// fakeLoader is an AuthzLoader stub returning fixed authorization inputs, so the
// chain tests exercise the wiring (RequestContext construction, context
// attachment, guard decisions) independent of the database-backed loader, which
// has its own tests.
type fakeLoader struct {
	groups []data.GroupID
	authz  service.AuthzData
	err    error
}

func (f fakeLoader) Load(_ context.Context, _ data.RequestContext) ([]data.GroupID, service.AuthzData, error) {
	return f.groups, f.authz, f.err
}

// decodeError reads the standard error envelope from a response body.
func decodeError(t *testing.T, body []byte) errorPayload {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode error envelope: %v (body=%q)", err, body)
	}
	return b.Error
}

// newTestChain builds a Chain with fake session + loader collaborators and the
// real PolicyEngine. TLS-enforce is off so tests need not set up TLS.
func newTestChain(sessions SessionValidator, loader AuthzLoader) *Chain {
	return NewChain(false, sessions, loader, service.NewPolicyEngine())
}

// okHandler records that it ran and echoes the RequestContext for assertions.
func okHandler(seen *data.RequestContext) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rc, ok := RequestContextFrom(r.Context()); ok {
			*seen = rc
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestChain_NoCookie_Returns401(t *testing.T) {
	chain := newTestChain(fakeSessions{validToken: "tok"}, fakeLoader{})
	var seen data.RequestContext
	h := chain.Handler(okHandler(&seen))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeError(t, rec.Body.Bytes()).Code; code != "UNAUTHENTICATED" {
		t.Errorf("error code = %q, want UNAUTHENTICATED", code)
	}
}

func TestChain_InvalidSession_Returns401(t *testing.T) {
	chain := newTestChain(fakeSessions{validToken: "good"}, fakeLoader{})
	var seen data.RequestContext
	ran := false
	h := chain.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		_ = seen
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "expired-or-forged"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ran {
		t.Error("downstream handler ran for an invalid session")
	}
}

func TestChain_ValidSession_AttachesRequestContext(t *testing.T) {
	// Groups 10 and 11; group 10 has read on Sphere 5, group 11 has write+reveal
	// on Sphere 5 and read (no reveal) on Sphere 6. The effective grant for
	// Sphere 5 should be write + reveal (most-permissive union), for Sphere 6
	// read + no reveal.
	loader := fakeLoader{
		groups: []data.GroupID{10, 11},
		authz: service.AuthzData{
			Grants: []service.GroupGrant{
				{Group: 10, Sphere: 5, Access: data.AccessRead},
				{Group: 11, Sphere: 5, Access: data.AccessWrite, Reveal: true},
				{Group: 11, Sphere: 6, Access: data.AccessRead},
			},
		},
	}
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{UserID: 42, TenantID: 7}},
		loader,
	)

	var seen data.RequestContext
	h := chain.Handler(okHandler(&seen))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen.UserID() != 42 {
		t.Errorf("UserID = %d, want 42", seen.UserID())
	}
	if seen.TenantID() != 7 {
		t.Errorf("TenantID = %d, want 7 (from session, not client input)", seen.TenantID())
	}

	g5, ok := seen.SphereGrant(5)
	if !ok {
		t.Fatal("expected an effective grant for Sphere 5")
	}
	if g5.Access != data.AccessWrite {
		t.Errorf("Sphere 5 access = %v, want write (union)", g5.Access)
	}
	if !g5.Reveal {
		t.Error("Sphere 5 reveal = false, want true (any group with reveal)")
	}

	g6, ok := seen.SphereGrant(6)
	if !ok {
		t.Fatal("expected an effective grant for Sphere 6")
	}
	if g6.Access != data.AccessRead || g6.Reveal {
		t.Errorf("Sphere 6 grant = %+v, want read without reveal", g6)
	}
}

func TestChain_LoaderError_Returns500(t *testing.T) {
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{UserID: 1, TenantID: 1}},
		fakeLoader{err: errors.New("boom")},
	)
	h := chain.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran despite loader failure")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestRequireAdmin_DeniesNonAdmin asserts a non-admin hitting an admin route
// gets 403 ADMIN_REQUIRED with the "admin privileges required" message
// (Req 5.5), and the admin handler never runs.
func TestRequireAdmin_DeniesNonAdmin(t *testing.T) {
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{UserID: 1, TenantID: 1}},
		fakeLoader{groups: []data.GroupID{10}, authz: service.AuthzData{Admin: false}},
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
		t.Error("admin handler ran for a non-admin")
	}
}

// TestRequireAdmin_AllowsAdmin asserts an active Admin_Group member passes the
// admin guard.
func TestRequireAdmin_AllowsAdmin(t *testing.T) {
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{UserID: 1, TenantID: 1}},
		fakeLoader{groups: []data.GroupID{99}, authz: service.AuthzData{Admin: true}},
	)

	adminRan := false
	admin := chain.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminRan = true
		w.WriteHeader(http.StatusOK)
	}))
	h := chain.Handler(admin)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !adminRan {
		t.Fatalf("status = %d, adminRan = %v, want 200 and true", rec.Code, adminRan)
	}
}

// TestRequireAdmin_DeactivatedAdminDenied asserts a deactivated admin is denied
// the admin pages (Req 5.4) while their memberships are retained.
func TestRequireAdmin_DeactivatedAdminDenied(t *testing.T) {
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{UserID: 1, TenantID: 1}},
		fakeLoader{groups: []data.GroupID{99}, authz: service.AuthzData{Admin: true, Deactivated: true}},
	)
	admin := chain.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("deactivated admin reached the admin handler")
	}))
	h := chain.Handler(admin)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if decodeError(t, rec.Body.Bytes()).Code != "ADMIN_REQUIRED" {
		t.Errorf("want ADMIN_REQUIRED for deactivated admin")
	}
}

// TestChain_MustEnrolSession_GatesToEnrolmentRoutes asserts a restricted
// 'must_enrol' session is allowed to reach ONLY the enrolment routes
// (POST /api/2fa/enrol, POST /api/2fa/confirm) and is refused with 403
// ENROLMENT_REQUIRED on every other route (Req 33.7, 33.8). Logout sits outside
// the chain, so it is not exercised here.
func TestChain_MustEnrolSession_GatesToEnrolmentRoutes(t *testing.T) {
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{
			UserID: 1, TenantID: 1, State: service.SessionStateMustEnrol,
		}},
		fakeLoader{},
	)

	do := func(method, path string) (*httptest.ResponseRecorder, *bool) {
		ran := false
		h := chain.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ran = true
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "tok"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec, &ran
	}

	allowed := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/2fa/enrol"},
		{http.MethodPost, "/api/2fa/confirm"},
	}
	for _, tc := range allowed {
		rec, ran := do(tc.method, tc.path)
		if rec.Code != http.StatusOK || !*ran {
			t.Errorf("%s %s: status=%d ran=%v, want 200 and handler to run", tc.method, tc.path, rec.Code, *ran)
		}
	}

	blocked := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/spheres"},
		{http.MethodGet, "/api/2fa/enrol"}, // wrong method on an allowed path
		{http.MethodPut, "/api/admin/2fa-policy"},
	}
	for _, tc := range blocked {
		rec, ran := do(tc.method, tc.path)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status=%d, want 403", tc.method, tc.path, rec.Code)
		}
		if code := decodeError(t, rec.Body.Bytes()).Code; code != "ENROLMENT_REQUIRED" {
			t.Errorf("%s %s: error code=%q, want ENROLMENT_REQUIRED", tc.method, tc.path, code)
		}
		if *ran {
			t.Errorf("%s %s: handler ran for a must_enrol session", tc.method, tc.path)
		}
	}
}

// TestChain_ActiveSession_NotGated asserts a normal 'active' session is not
// subject to the enrolment gate and reaches an ordinary route (Req 33.7).
func TestChain_ActiveSession_NotGated(t *testing.T) {
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{
			UserID: 1, TenantID: 1, State: service.SessionStateActive,
		}},
		fakeLoader{},
	)
	ran := false
	h := chain.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/spheres", nil)
	req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !ran {
		t.Fatalf("status=%d ran=%v, want 200 and handler to run", rec.Code, ran)
	}
}

// TestRequireSphereAccess mounts the Sphere guard on a chi route so chi.URLParam
// resolves the {sphereID} placeholder, then asserts allow/deny outcomes.
func TestRequireSphereAccess(t *testing.T) {
	chain := newTestChain(
		fakeSessions{validToken: "tok", session: service.Session{UserID: 1, TenantID: 1}},
		fakeLoader{
			groups: []data.GroupID{10},
			authz: service.AuthzData{Grants: []service.GroupGrant{
				{Group: 10, Sphere: 5, Access: data.AccessRead},
			}},
		},
	)

	newRouter := func(min data.AccessLevel, ran *bool) http.Handler {
		r := chi.NewRouter()
		r.Use(chain.Middleware())
		r.With(chain.RequireSphereAccess("sphereID", min)).
			Get("/spheres/{sphereID}", func(w http.ResponseWriter, req *http.Request) {
				*ran = true
				w.WriteHeader(http.StatusOK)
			})
		return r
	}

	do := func(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: "tok"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("read access granted", func(t *testing.T) {
		var ran bool
		rec := do(t, newRouter(data.AccessRead, &ran), "/spheres/5")
		if rec.Code != http.StatusOK || !ran {
			t.Fatalf("status = %d, ran = %v, want 200 and true", rec.Code, ran)
		}
	})

	t.Run("write required but only read granted", func(t *testing.T) {
		var ran bool
		rec := do(t, newRouter(data.AccessWrite, &ran), "/spheres/5")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if decodeError(t, rec.Body.Bytes()).Code != "NO_SPHERE_ACCESS" {
			t.Errorf("want NO_SPHERE_ACCESS code")
		}
		if ran {
			t.Error("handler ran without write access")
		}
	})

	t.Run("no grant for sphere", func(t *testing.T) {
		var ran bool
		rec := do(t, newRouter(data.AccessRead, &ran), "/spheres/999")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if ran {
			t.Error("handler ran for a sphere with no grant")
		}
	})
}
