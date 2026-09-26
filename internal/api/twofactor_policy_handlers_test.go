package api

// Integration tests for the admin tenant Two-Factor_Authentication policy
// toggle (PUT /api/admin/2fa-policy), driven end to end through Server.Router()
// and the real middleware chain over a real Central_Directory (newTestEnv).
// They prove the route is mounted inside the RequireAdmin group, delegates to
// TwoFactorService, persists the flag for the caller's tenant only, and shapes
// the response (Req 33.7, 33.8, 32.6).

import (
	"net/http"
	"testing"
)

// TestSetTwoFactorPolicyRequiresAdmin verifies a non-admin session is refused
// with 403 ADMIN_REQUIRED and the policy is not written (Req 33.7, 5.5).
func TestSetTwoFactorPolicyRequiresAdmin(t *testing.T) {
	e := newTestEnv(t)

	rec := e.do(t, http.MethodPut, "/api/admin/2fa-policy", e.sessionToken, twoFactorPolicyRequest{Required: true})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeEnvelopeCode(t, rec); code != codeAdminRequired {
		t.Errorf("error code = %q, want %q", code, codeAdminRequired)
	}
	// No policy row was written for tenant 1.
	var n int
	if err := e.central.QueryRow(`SELECT COUNT(*) FROM "TWO_FACTOR_POLICY" WHERE tenant_id = 1`).Scan(&n); err != nil {
		t.Fatalf("count policy rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("policy rows = %d, want 0 (non-admin must not write)", n)
	}
}

// TestSetTwoFactorPolicyAdminTogglesTenant verifies an admin can set the policy
// required and back to optional, and that the write is scoped to the caller's
// tenant (tenant 1) — persisted as required=1 then required=0 (Req 33.7, 33.8).
func TestSetTwoFactorPolicyAdminTogglesTenant(t *testing.T) {
	e := newTestEnv(t)

	// Set required.
	rec := e.do(t, http.MethodPut, "/api/admin/2fa-policy", e.adminToken, twoFactorPolicyRequest{Required: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("set-required status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := tenantPolicyRequired(t, e); !got {
		t.Fatal("tenant 1 policy should be required after set-required")
	}

	// Toggle back to optional; the same row is updated in place.
	rec = e.do(t, http.MethodPut, "/api/admin/2fa-policy", e.adminToken, twoFactorPolicyRequest{Required: false})
	if rec.Code != http.StatusOK {
		t.Fatalf("set-optional status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := tenantPolicyRequired(t, e); got {
		t.Fatal("tenant 1 policy should be optional after set-optional")
	}

	// Exactly one policy row exists for the tenant — an upsert, not a duplicate.
	var n int
	if err := e.central.QueryRow(`SELECT COUNT(*) FROM "TWO_FACTOR_POLICY" WHERE tenant_id = 1`).Scan(&n); err != nil {
		t.Fatalf("count policy rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("policy rows for tenant 1 = %d, want 1", n)
	}
}

// tenantPolicyRequired reads tenant 1's persisted required flag directly.
func tenantPolicyRequired(t *testing.T, e *testEnv) bool {
	t.Helper()
	var required int
	err := e.central.QueryRow(`SELECT required FROM "TWO_FACTOR_POLICY" WHERE tenant_id = 1`).Scan(&required)
	if err != nil {
		t.Fatalf("read tenant policy: %v", err)
	}
	return required != 0
}
