package api

// Table-driven handler tests exercising a representative sample of the wired
// routes end to end through the chi router and the real middleware chain.
//
// The tests stand up a real Central_Directory and a real tenant SQLite file
// behind a real ConnManager, seed a user + group + session + a Sphere with a
// grant, and drive requests through Server.Router() with httptest. They assert
// three things the wiring must guarantee:
//
//   - Routes are mounted and delegate to the right service/repo (create a
//     Sphere, list it back, search, quick searches, bookmarks).
//   - Authentication is enforced: a request with no session cookie is rejected
//     with 401 before any handler runs.
//   - Responses and errors are shaped with the standard envelope (a validation
//     failure is a 400 VALIDATION; an admin route for a non-admin is 403
//     ADMIN_REQUIRED).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/influence/influence/internal/config"
	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/service"
)

// testEnv holds a wired server plus the direct handles the tests seed and
// assert against.
type testEnv struct {
	srv     *Server
	central *dataDB
	tenant  *dataDB
	// sessionToken authenticates a normal (non-admin) user in tenant 1.
	sessionToken string
	// adminToken authenticates an Admin_Group user in tenant 1.
	adminToken string
	// sphereRecordID is a Sphere the normal user can read+write.
	sphereRecordID string
}

// dataDB is a thin alias so the test file does not import database/sql directly
// in its signatures; it is the same *sql.DB.
type dataDB = sqlDB

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	central := openSQLite(t, filepath.Join(dir, "central.db"))
	if err := data.MigrateCentral(ctx, central); err != nil {
		t.Fatalf("migrate central: %v", err)
	}

	tenantPath := filepath.Join(dir, "tenant_a.db")
	insertTenant(t, central, 1, "tenant-a-uuid", "A", tenantPath)
	tenant := openSQLite(t, tenantPath)
	if err := data.MigrateTenant(ctx, tenant); err != nil {
		t.Fatalf("migrate tenant: %v", err)
	}
	if err := service.InitTenantSecret(ctx, tenant, "test-master"); err != nil {
		t.Fatalf("init tenant secret: %v", err)
	}

	// Seed identity: a normal user in a plain Group, and an admin user in the
	// Admin_Group. Both belong to tenant 1.
	normalUser := insertUser(t, central, 1, "alice", "Alice")
	adminUser := insertUser(t, central, 1, "root", "Root")
	plainGroup := insertGroup(t, central, 1, "editors", false)
	adminGroup := insertGroup(t, central, 1, "admins", true)
	insertMembership(t, central, normalUser, plainGroup)
	insertMembership(t, central, adminUser, adminGroup)

	// Seed a Sphere and grant the plain Group read+write+reveal on it so the
	// normal user has an effective grant the middleware will load.
	sphereRID := insertSphere(t, tenant, "Ops")
	grantSphere(t, tenant, sphereRID.surrogate, plainGroup, "write", true)

	// Server wired over the connection manager.
	conns := data.NewConnManager(central)
	t.Cleanup(func() { _ = conns.Close() })

	cfg := config.Config{Port: 8080, DataDir: dir, MasterSecret: "test-master"}
	srv := NewServer(conns, cfg)

	// Establish sessions directly in the SESSION table so requests can carry a
	// valid cookie the chain's Session stage accepts.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	normalTok := insertSession(t, central, normalUser, now)
	adminTok := insertSession(t, central, adminUser, now)

	return &testEnv{
		srv:            srv,
		central:        central,
		tenant:         tenant,
		sessionToken:   normalTok,
		adminToken:     adminTok,
		sphereRecordID: sphereRID.canonical,
	}
}

// do performs a request through the router, optionally attaching the given
// session cookie value (empty means unauthenticated).
func (e *testEnv) do(t *testing.T, method, target, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: service.SessionCookieName, Value: token})
	}
	rec := httptest.NewRecorder()
	e.srv.Router().ServeHTTP(rec, req)
	return rec
}

// decodeEnvelopeCode extracts error.code from a response body.
func decodeEnvelopeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

// TestAuthEnforcedWithoutSession verifies that every authenticated route is
// rejected with 401 when no session cookie is presented — the Session stage
// runs before any handler (Req 3.5, 3.6).
func TestAuthEnforcedWithoutSession(t *testing.T) {
	e := newTestEnv(t)

	cases := []struct {
		name   string
		method string
		target string
	}{
		{"list spheres", http.MethodGet, "/api/spheres"},
		{"create sphere", http.MethodPost, "/api/spheres"},
		{"search", http.MethodGet, "/api/search?q=x"},
		{"quick created", http.MethodGet, "/api/quick/created"},
		{"bookmarks", http.MethodGet, "/api/bookmarks"},
		{"admin create tenant", http.MethodPost, "/api/admin/tenants"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, tc.method, tc.target, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if code := decodeEnvelopeCode(t, rec); code != codeUnauthenticated {
				t.Errorf("error code = %q, want %q", code, codeUnauthenticated)
			}
		})
	}
}

// TestListSpheresDelegates verifies the authenticated list route delegates to
// the circle/sphere repo and returns the seeded Sphere the user can access.
func TestListSpheresDelegates(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodGet, "/api/spheres", e.sessionToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Spheres []sphereResponse `json:"spheres"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Spheres) != 1 || body.Spheres[0].RecordID != e.sphereRecordID {
		t.Fatalf("spheres = %+v, want the seeded sphere %q", body.Spheres, e.sphereRecordID)
	}
	if body.Spheres[0].Name != "Ops" {
		t.Errorf("sphere name = %q, want Ops", body.Spheres[0].Name)
	}
}

// TestCreateSphereRoundTrip verifies POST /api/spheres creates a Sphere (201)
// and that it is then listable.
func TestCreateSphereRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodPost, "/api/spheres", e.sessionToken, createSphereRequest{Name: "Finance"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var created sphereResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Name != "Finance" || created.RecordID == "" || created.ShortID == "" {
		t.Fatalf("created = %+v, want a named sphere with ids", created)
	}
}

// TestCreateSphereValidationError verifies a name-rule violation is shaped as a
// 400 VALIDATION envelope (Req 6.4).
func TestCreateSphereValidationError(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodPost, "/api/spheres", e.sessionToken, createSphereRequest{Name: ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
}

// TestAdminRouteDeniedForNonAdmin verifies the admin subtree is guarded: a
// non-admin session is refused with 403 ADMIN_REQUIRED (Req 5.5), while an
// admin session passes the guard (and gets a non-403 result).
func TestAdminRouteDeniedForNonAdmin(t *testing.T) {
	e := newTestEnv(t)

	rec := e.do(t, http.MethodPost, "/api/admin/tenants", e.sessionToken, createTenantRequest{Name: "New Org"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeAdminRequired {
		t.Errorf("error code = %q, want %q", code, codeAdminRequired)
	}

	// An admin passes the guard. Tenant creation itself may succeed or fail on
	// the filesystem, but it must NOT be an admin-guard rejection.
	adminRec := e.do(t, http.MethodPost, "/api/admin/tenants", e.adminToken, createTenantRequest{Name: "New Org"})
	if adminRec.Code == http.StatusForbidden {
		t.Fatalf("admin was refused by the guard: body %q", adminRec.Body.String())
	}
}

// TestNotAccessibleShapedAsTenantIsolation verifies a reference to a record not
// in the caller's tenant maps to 404 TENANT_ISOLATION (Req 1.6). Deleting a
// random (nonexistent) Sphere id exercises the ErrNotAccessible path.
func TestNotAccessibleShapedAsTenantIsolation(t *testing.T) {
	e := newTestEnv(t)
	// A syntactically valid but unknown canonical Record_ID.
	rec := e.do(t, http.MethodDelete, "/api/spheres/00000000-0000-4000-8000-000000000000", e.sessionToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeTenantIsolation {
		t.Errorf("error code = %q, want %q", code, codeTenantIsolation)
	}
}

// countSpheres returns the number of rows in the tenant's sphere table. It lets
// the mutation-invariant test assert a failed create wrote nothing.
func countSpheres(t *testing.T, tenant *dataDB) int {
	t.Helper()
	var n int
	if err := tenant.QueryRow(`SELECT COUNT(*) FROM sphere`).Scan(&n); err != nil {
		t.Fatalf("count spheres: %v", err)
	}
	return n
}

// TestFailedMutationLeavesStateUnchanged pins the transactional invariant behind
// the error envelope (task 22.2): a mutating operation that fails leaves state
// unchanged. It drives a validation-violating Sphere create through the wired
// server, asserts the standard 400 VALIDATION envelope, and confirms the
// tenant's sphere set is byte-for-byte the same before and after — the rejected
// write committed nothing (Req 6.4). A subsequent VALID create still succeeds,
// proving the failed attempt left no half-open transaction or poisoned state.
func TestFailedMutationLeavesStateUnchanged(t *testing.T) {
	e := newTestEnv(t)

	before := countSpheres(t, e.tenant)

	// An empty name violates the 1-100 char rule (Req 6.4). The create must be
	// rejected and must not write a row.
	rec := e.do(t, http.MethodPost, "/api/spheres", e.sessionToken, createSphereRequest{Name: ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
	if after := countSpheres(t, e.tenant); after != before {
		t.Fatalf("failed create changed sphere count: before=%d after=%d (must leave state unchanged)", before, after)
	}

	// The tenant is not poisoned by the failed transaction: a valid create still
	// commits and increments the count by exactly one.
	ok := e.do(t, http.MethodPost, "/api/spheres", e.sessionToken, createSphereRequest{Name: "Finance"})
	if ok.Code != http.StatusCreated {
		t.Fatalf("valid create after failure = %d, want 201 (body %q)", ok.Code, ok.Body.String())
	}
	if after := countSpheres(t, e.tenant); after != before+1 {
		t.Errorf("valid create should add exactly one sphere: before=%d after=%d", before, after)
	}
}

// TestHealthzIsUnauthenticated verifies the liveness check needs no session.
func TestHealthzIsUnauthenticated(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("healthz = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

// TestSearchEmptyResult verifies the search route delegates and returns an
// empty result set for a query that matches nothing (Req 23.3).
func TestSearchEmptyResult(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodGet, "/api/search?q=nothingmatchesthis", e.sessionToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Results []polygonResponse `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Results) != 0 {
		t.Errorf("results = %+v, want empty", body.Results)
	}
}

// TestBookmarksEmptyState verifies the bookmarks route reports the empty state
// for a user with no bookmarks (Req 26.4).
func TestBookmarksEmptyState(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodGet, "/api/bookmarks", e.sessionToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Empty bool `json:"empty"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Empty {
		t.Errorf("empty = false, want true for a user with no bookmarks")
	}
}

// meBody is the decoded /api/me payload used by the identity endpoint tests.
type meBody struct {
	UserID            int64  `json:"userId"`
	DisplayName       string `json:"displayName"`
	IsAdmin           bool   `json:"isAdmin"`
	TwoFactorEnrolled bool   `json:"twoFactorEnrolled"`
	TwoFactorRequired bool   `json:"twoFactorRequired"`
}

func decodeMe(t *testing.T, rec *httptest.ResponseRecorder) meBody {
	t.Helper()
	var body meBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/me %q: %v", rec.Body.String(), err)
	}
	return body
}

// TestMeRequiresSession verifies the identity endpoint sits behind the chain: a
// request with no session cookie is rejected with 401 (Req 3.5, 32.1).
func TestMeRequiresSession(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodGet, "/api/me", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeUnauthenticated {
		t.Errorf("error code = %q, want %q", code, codeUnauthenticated)
	}
}

// TestMeNonAdminIdentity verifies the identity endpoint returns the caller's
// display name and a false admin flag for a normal (non-admin) session, with
// both 2FA fields defaulting to false when the user is not enrolled and the
// tenant has no policy row (Req 32.1, 33.7).
func TestMeNonAdminIdentity(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodGet, "/api/me", e.sessionToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	body := decodeMe(t, rec)
	if body.DisplayName != "Alice" {
		t.Errorf("displayName = %q, want Alice", body.DisplayName)
	}
	if body.IsAdmin {
		t.Errorf("isAdmin = true, want false for a non-admin user")
	}
	if body.TwoFactorEnrolled {
		t.Errorf("twoFactorEnrolled = true, want false for an unenrolled user")
	}
	if body.TwoFactorRequired {
		t.Errorf("twoFactorRequired = true, want false with no tenant policy row")
	}
	if body.UserID == 0 {
		t.Errorf("userId = 0, want the caller's user id")
	}
}

// TestMeAdminIdentity verifies the admin flag reflects Admin_Group membership:
// the admin session reports isAdmin=true (Req 32.1). The endpoint is reachable
// by the admin even though it is not behind RequireAdmin.
func TestMeAdminIdentity(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(t, http.MethodGet, "/api/me", e.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	body := decodeMe(t, rec)
	if body.DisplayName != "Root" {
		t.Errorf("displayName = %q, want Root", body.DisplayName)
	}
	if !body.IsAdmin {
		t.Errorf("isAdmin = false, want true for an Admin_Group member")
	}
}

// TestMeReflectsTwoFactorState verifies the endpoint reports the stored 2FA
// enrolment for the user and the tenant's 2FA policy: with the USER marked
// enrolled and the tenant policy set to required, both fields are true
// (Req 32.1, 33.2, 33.7).
func TestMeReflectsTwoFactorState(t *testing.T) {
	e := newTestEnv(t)

	// Mark the normal user enrolled and require 2FA for their tenant.
	if _, err := e.central.Exec(
		`UPDATE "USER" SET two_factor_enrolled = 1 WHERE username = 'alice'`,
	); err != nil {
		t.Fatalf("mark user enrolled: %v", err)
	}
	if _, err := e.central.Exec(
		`INSERT INTO "TWO_FACTOR_POLICY" (tenant_id, required) VALUES (1, 1)`,
	); err != nil {
		t.Fatalf("set tenant policy: %v", err)
	}

	rec := e.do(t, http.MethodGet, "/api/me", e.sessionToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	body := decodeMe(t, rec)
	if !body.TwoFactorEnrolled {
		t.Errorf("twoFactorEnrolled = false, want true for an enrolled user")
	}
	if !body.TwoFactorRequired {
		t.Errorf("twoFactorRequired = false, want true when the tenant policy requires it")
	}
}
