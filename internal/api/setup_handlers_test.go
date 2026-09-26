package api

// Integration tests for the first-run setup routes wired in task 28.2 (Req 31).
// They drive requests through Server.Router() exactly as a browser would,
// exercising the two UNAUTHENTICATED endpoints that sit outside the middleware
// chain:
//
//   - GET  /api/setup/state — reports First_Run_State without a session.
//   - POST /api/setup/admin — creates the Bootstrap_Admin in one transaction,
//     then refuses further calls with SETUP_COMPLETE (Req 31.4, 31.6).
//
// The setup flow is pure Central_Directory identity work, so these tests need
// no tenant content database: a migrated Central with a single active tenant
// (and no admin yet) is the whole fixture. That fresh-tenant state is exactly
// First_Run_State, which is why newTestEnv (whose tenant 1 already has an admin)
// is not reused here.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/influence/influence/internal/config"
	"github.com/influence/influence/internal/data"
)

// setupEnv is a minimal wired server over a Central_Directory holding a single
// active tenant with NO administrator — i.e. a tenant in First_Run_State.
type setupEnv struct {
	srv     *Server
	central *dataDB
}

// newSetupEnv migrates a fresh Central, inserts one active tenant (id 1) with no
// users or groups, and wires a Server over it. No tenant database is created
// because the setup endpoints never touch tenant content.
func newSetupEnv(t *testing.T) *setupEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	central := openSQLite(t, filepath.Join(dir, "central.db"))
	if err := data.MigrateCentral(ctx, central); err != nil {
		t.Fatalf("migrate central: %v", err)
	}
	// A tenant with no admin member: First_Run_State holds. db_path is unused —
	// the setup flow is Central-only.
	insertTenant(t, central, 1, "tenant-a-uuid", "A", "unused")

	conns := data.NewConnManager(central)
	t.Cleanup(func() { _ = conns.Close() })

	cfg := config.Config{Port: 8080, DataDir: dir, MasterSecret: "test-master"}
	return &setupEnv{srv: NewServer(conns, cfg), central: central}
}

// do performs an unauthenticated request through the router (setup carries no
// session cookie).
func (e *setupEnv) do(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doNoCookie(t, e.srv, method, target, body)
}

// doNoCookie issues a request through the router with no session cookie,
// marshaling body to JSON when non-nil.
func doNoCookie(t *testing.T, srv *Server, method, target string, body any) *httptest.ResponseRecorder {
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
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	return rec
}

// TestSetupStateReportsFirstRunThenClears asserts the state endpoint reports
// firstRun=true for a fresh tenant and firstRun=false once an admin exists
// (Req 31.1, 31.3), all without any session cookie.
func TestSetupStateReportsFirstRunThenClears(t *testing.T) {
	e := newSetupEnv(t)

	rec := e.do(t, http.MethodGet, "/api/setup/state", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("state status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := decodeFirstRun(t, rec); !got {
		t.Fatal("fresh tenant: firstRun = false, want true")
	}

	// Create the Bootstrap_Admin, then the state must flip.
	createRec := e.do(t, http.MethodPost, "/api/setup/admin", setupAdminRequest{
		Username: "boss",
		Password: "Sup3rSecret!pw",
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %q)", createRec.Code, createRec.Body.String())
	}

	after := e.do(t, http.MethodGet, "/api/setup/state", nil)
	if after.Code != http.StatusOK {
		t.Fatalf("post-create state status = %d, want 200 (body %q)", after.Code, after.Body.String())
	}
	if got := decodeFirstRun(t, after); got {
		t.Fatal("after create: firstRun = true, want false")
	}
}

// TestSetupAdminCreatesAdmin asserts POST /api/setup/admin creates a user that
// is a member of the tenant's is_admin Group, storing an Argon2id hash (Req
// 31.2, 3.4).
func TestSetupAdminCreatesAdmin(t *testing.T) {
	e := newSetupEnv(t)

	rec := e.do(t, http.MethodPost, "/api/setup/admin", setupAdminRequest{
		Username: "boss",
		Password: "Sup3rSecret!pw",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		UserID   int64 `json:"userId"`
		FirstRun bool  `json:"firstRun"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if resp.UserID == 0 || resp.FirstRun {
		t.Fatalf("response = %+v, want non-zero userId and firstRun=false", resp)
	}

	// The created user is an admin member and its hash is Argon2id, not plaintext.
	var admins int
	if err := e.central.QueryRow(
		`SELECT COUNT(*) FROM "GROUP_MEMBERSHIP" gm
		   JOIN "GROUP" g ON g.id = gm.group_id
		  WHERE gm.user_id = ? AND g.is_admin = 1`,
		resp.UserID,
	).Scan(&admins); err != nil {
		t.Fatalf("count admin membership: %v", err)
	}
	if admins != 1 {
		t.Fatalf("admin memberships for created user = %d, want 1", admins)
	}
	var hash string
	if err := e.central.QueryRow(`SELECT password_hash FROM "USER" WHERE id = ?`, resp.UserID).Scan(&hash); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if hash == "Sup3rSecret!pw" || len(hash) < len("$argon2id$") || hash[:len("$argon2id$")] != "$argon2id$" {
		t.Fatalf("password_hash %q is not an Argon2id hash", hash)
	}
}

// TestSetupAdminSecondCallIsSetupComplete asserts a second create is rejected
// with 409 SETUP_COMPLETE and leaves the user set unchanged (Req 31.4).
func TestSetupAdminSecondCallIsSetupComplete(t *testing.T) {
	e := newSetupEnv(t)

	first := e.do(t, http.MethodPost, "/api/setup/admin", setupAdminRequest{Username: "boss", Password: "Sup3rSecret!pw"})
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201 (body %q)", first.Code, first.Body.String())
	}

	second := e.do(t, http.MethodPost, "/api/setup/admin", setupAdminRequest{Username: "usurper", Password: "Sup3rSecret!pw"})
	if second.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409 (body %q)", second.Code, second.Body.String())
	}
	if code := decodeEnvelopeCode(t, second); code != codeSetupComplete {
		t.Errorf("error code = %q, want %q", code, codeSetupComplete)
	}
	var users int
	if err := e.central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE tenant_id = 1`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Fatalf("user count = %d, want 1 (second create must persist nothing)", users)
	}
}

// TestSetupAdminWeakPasswordIsValidation asserts a weak password is a 400
// VALIDATION, creates no user, and leaves the tenant in First_Run_State (Req
// 31.5).
func TestSetupAdminWeakPasswordIsValidation(t *testing.T) {
	e := newSetupEnv(t)

	rec := e.do(t, http.MethodPost, "/api/setup/admin", setupAdminRequest{Username: "boss", Password: "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
	var users int
	if err := e.central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE tenant_id = 1`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 0 {
		t.Fatalf("weak-password create persisted %d users, want 0", users)
	}

	// Still first-run.
	state := e.do(t, http.MethodGet, "/api/setup/state", nil)
	if !decodeFirstRun(t, state) {
		t.Fatal("tenant should still be in First_Run_State after a rejected create")
	}
}

// TestSetupAdminEmptyUsernameIsValidation asserts an empty username is a 400
// VALIDATION and creates no user (Req 31.5).
func TestSetupAdminEmptyUsernameIsValidation(t *testing.T) {
	e := newSetupEnv(t)

	rec := e.do(t, http.MethodPost, "/api/setup/admin", setupAdminRequest{Username: "", Password: "Sup3rSecret!pw"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeValidation {
		t.Errorf("error code = %q, want %q", code, codeValidation)
	}
	var users int
	if err := e.central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE tenant_id = 1`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 0 {
		t.Fatalf("empty-username create persisted %d users, want 0", users)
	}
}

// TestSetupStateUnknownTenantIsNotFound asserts an explicit tenant that does not
// exist is a 404 NOT_FOUND rather than reporting a state.
func TestSetupStateUnknownTenantIsNotFound(t *testing.T) {
	e := newSetupEnv(t)
	rec := e.do(t, http.MethodGet, "/api/setup/state?tenant=does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeNotFound {
		t.Errorf("error code = %q, want %q", code, codeNotFound)
	}
}

// decodeFirstRun extracts the firstRun flag from a setup-state response body.
func decodeFirstRun(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	var body setupStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode setup-state %q: %v", rec.Body.String(), err)
	}
	return body.FirstRun
}
