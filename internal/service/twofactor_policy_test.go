package service

// Tests for the tenant Two_Factor_Policy toggle and the login-time enrolment
// gating it drives (Req 33.7, 33.8). SetTenantPolicy upserts the per-tenant
// required flag scoped to the caller's tenant, and Login mints a 'must_enrol'
// session only when the tenant policy is required AND the user is not yet
// enrolled. All tests run over a real in-memory Central_Directory
// (newTestConns/seedTenant/seedUser from auth_test.go) — no fakes.

import (
	"context"
	"testing"

	"github.com/influence/influence/internal/data"
)

// tenantPolicyRow reads the persisted required flag for a tenant, and whether a
// TWO_FACTOR_POLICY row exists at all, straight from Central so a test asserts
// storage independent of the service.
func tenantPolicyRow(t *testing.T, svc *TwoFactorService, tenantID int64) (required bool, exists bool) {
	t.Helper()
	var req int
	err := svc.central.QueryRow(
		`SELECT required FROM "TWO_FACTOR_POLICY" WHERE tenant_id = ?`, tenantID,
	).Scan(&req)
	if err != nil {
		return false, false
	}
	return req != 0, true
}

// rcFor builds a minimal RequestContext carrying the caller's tenant, which is
// all SetTenantPolicy reads.
func rcFor(userID, tenantID int64) data.RequestContext {
	return data.NewRequestContext(data.UserID(userID), data.TenantID(tenantID), nil, nil)
}

// TestSetTenantPolicyUpsertsRequired verifies SetTenantPolicy creates the
// tenant's policy row on first call and flips the required flag in place on a
// subsequent call — a true upsert, never a duplicate row (Req 33.7, 33.8).
func TestSetTenantPolicyUpsertsRequired(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "admin", "hunter2xY!")
	svc := NewTwoFactorService(conns)
	rc := rcFor(userID, tenantID)

	// Required = true creates the row with required=1.
	if err := svc.SetTenantPolicy(context.Background(), rc, true); err != nil {
		t.Fatalf("set policy required: %v", err)
	}
	if required, exists := tenantPolicyRow(t, svc, tenantID); !exists || !required {
		t.Fatalf("after set-required: exists=%v required=%v, want true/true", exists, required)
	}

	// Toggling back to optional updates the same row in place.
	if err := svc.SetTenantPolicy(context.Background(), rc, false); err != nil {
		t.Fatalf("set policy optional: %v", err)
	}
	if required, exists := tenantPolicyRow(t, svc, tenantID); !exists || required {
		t.Fatalf("after set-optional: exists=%v required=%v, want true/false", exists, required)
	}

	// There must be exactly one row for the tenant — the upsert never inserts a
	// duplicate.
	var n int
	if err := svc.central.QueryRow(
		`SELECT COUNT(*) FROM "TWO_FACTOR_POLICY" WHERE tenant_id = ?`, tenantID,
	).Scan(&n); err != nil {
		t.Fatalf("count policy rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("policy rows for tenant = %d, want 1", n)
	}
}

// TestSetTenantPolicyIsTenantScoped verifies the policy is written for the
// caller's tenant only: setting tenant A's policy required leaves tenant B's
// policy untouched (Req 32.6, 33.7).
func TestSetTenantPolicyIsTenantScoped(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantA := seedTenant(t, central, "uuid-a", "Org A")
	tenantB := seedTenant(t, central, "uuid-b", "Org B")
	adminA := seedUser(t, central, tenantA, "admin-a", "hunter2xY!")
	svc := NewTwoFactorService(conns)

	if err := svc.SetTenantPolicy(context.Background(), rcFor(adminA, tenantA), true); err != nil {
		t.Fatalf("set tenant A policy: %v", err)
	}

	if required, exists := tenantPolicyRow(t, svc, tenantA); !exists || !required {
		t.Fatalf("tenant A: exists=%v required=%v, want true/true", exists, required)
	}
	// Tenant B has no policy row at all — the write was confined to tenant A.
	if _, exists := tenantPolicyRow(t, svc, tenantB); exists {
		t.Fatal("tenant B policy row exists; SetTenantPolicy leaked across tenants")
	}
}

// TestLoginMustEnrolWhenRequiredAndUnenrolled verifies that a correct password
// against a tenant with a required policy, for a user who has NOT enrolled,
// yields a restricted 'must_enrol' session (Req 33.7, 33.8).
func TestLoginMustEnrolWhenRequiredAndUnenrolled(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")

	twoFactor := NewTwoFactorService(conns)
	if err := twoFactor.SetTenantPolicy(context.Background(), rcFor(userID, tenantID), true); err != nil {
		t.Fatalf("set policy required: %v", err)
	}

	auth := NewAuthService(conns)
	sess, err := auth.Login(context.Background(), "alice", "hunter2xY!")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.State != SessionStateMustEnrol {
		t.Fatalf("session state = %q, want %q", sess.State, SessionStateMustEnrol)
	}

	// The persisted SESSION row carries the same restricted state, and
	// ValidateSession reads it back.
	validated, err := auth.ValidateSession(context.Background(), sess.Token)
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	if validated.State != SessionStateMustEnrol {
		t.Fatalf("validated state = %q, want %q", validated.State, SessionStateMustEnrol)
	}
}

// TestLoginActiveWhenPolicyOptional verifies that with no required policy an
// unenrolled user still logs in to a normal 'active' session (Req 33.7).
func TestLoginActiveWhenPolicyOptional(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	seedUser(t, central, tenantID, "alice", "hunter2xY!")

	auth := NewAuthService(conns)
	sess, err := auth.Login(context.Background(), "alice", "hunter2xY!")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.State != SessionStateActive {
		t.Fatalf("session state = %q, want %q (policy optional)", sess.State, SessionStateActive)
	}
}

// TestLoginActiveWhenRequiredButEnrolled verifies that a required policy does
// NOT restrict a user who is already enrolled: the enrolment gate only applies
// to unenrolled users (Req 33.7, 33.8).
func TestLoginActiveWhenRequiredButEnrolled(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	userID := seedUser(t, central, tenantID, "alice", "hunter2xY!")

	// Mark the user enrolled directly.
	if _, err := central.Exec(
		`UPDATE "USER" SET two_factor_enrolled = 1 WHERE id = ?`, userID,
	); err != nil {
		t.Fatalf("mark enrolled: %v", err)
	}

	twoFactor := NewTwoFactorService(conns)
	if err := twoFactor.SetTenantPolicy(context.Background(), rcFor(userID, tenantID), true); err != nil {
		t.Fatalf("set policy required: %v", err)
	}

	auth := NewAuthService(conns)
	sess, err := auth.Login(context.Background(), "alice", "hunter2xY!")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.State != SessionStateActive {
		t.Fatalf("session state = %q, want %q (enrolled user)", sess.State, SessionStateActive)
	}
}
