package service

import (
	"testing"

	"github.com/influence/influence/internal/data"
)

// baseCtx is a minimal RequestContext for the policy tests. The PolicyEngine's
// decisions derive entirely from the AuthzData argument, so the context only
// needs to be a valid identity; the grants it carries are not consulted by the
// engine.
func baseCtx() data.RequestContext {
	return data.NewRequestContext(1, 1, []data.GroupID{10, 11}, nil)
}

// TestCanAccessSphere_UnionMostPermissive verifies that effective access is the
// most-permissive level across the caller's Groups (Req 4.3): a read grant from
// one Group and a write grant from another on the same Sphere yield write,
// regardless of the order the grants appear in.
func TestCanAccessSphere_UnionMostPermissive(t *testing.T) {
	eng := NewPolicyEngine()

	cases := []struct {
		name   string
		grants []GroupGrant
	}{
		{
			name: "read then write",
			grants: []GroupGrant{
				{Group: 10, Sphere: 5, Access: data.AccessRead},
				{Group: 11, Sphere: 5, Access: data.AccessWrite},
			},
		},
		{
			name: "write then read (order independent)",
			grants: []GroupGrant{
				{Group: 11, Sphere: 5, Access: data.AccessWrite},
				{Group: 10, Sphere: 5, Access: data.AccessRead},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eng.CanAccessSphere(baseCtx(), AuthzData{Grants: tc.grants}, 5)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != data.AccessWrite {
				t.Errorf("CanAccessSphere = %v, want AccessWrite", got)
			}
		})
	}
}

// TestCanAccessSphere_ReadOnly verifies that when the only grant is read, the
// effective access is read (no phantom write).
func TestCanAccessSphere_ReadOnly(t *testing.T) {
	eng := NewPolicyEngine()
	authz := AuthzData{Grants: []GroupGrant{
		{Group: 10, Sphere: 5, Access: data.AccessRead},
	}}

	got, err := eng.CanAccessSphere(baseCtx(), authz, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != data.AccessRead {
		t.Errorf("CanAccessSphere = %v, want AccessRead", got)
	}
}

// TestCanAccessSphere_NoGrantDenied verifies that a caller with no Group
// granting access to the requested Sphere gets AccessNone (Req 4.5). Grants for
// a different Sphere must not leak access to this one.
func TestCanAccessSphere_NoGrantDenied(t *testing.T) {
	eng := NewPolicyEngine()
	authz := AuthzData{Grants: []GroupGrant{
		{Group: 10, Sphere: 99, Access: data.AccessWrite, Reveal: true},
	}}

	got, err := eng.CanAccessSphere(baseCtx(), authz, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != data.AccessNone {
		t.Errorf("CanAccessSphere = %v, want AccessNone", got)
	}
}

// TestCanAccessSphere_AdminImplicitWrite verifies that an Admin_Group member has
// write access to any Sphere in the tenant even with no explicit grant (Req 4.7).
func TestCanAccessSphere_AdminImplicitWrite(t *testing.T) {
	eng := NewPolicyEngine()
	authz := AuthzData{Admin: true} // no grants at all

	got, err := eng.CanAccessSphere(baseCtx(), authz, 12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != data.AccessWrite {
		t.Errorf("admin CanAccessSphere = %v, want AccessWrite", got)
	}
}

// TestCanAccessSphere_DeactivatedDeniedEvenAdmin verifies that a deactivated
// user is denied every Sphere, and that the deactivated check overrides the
// admin implicit grant (Req 5.4 takes precedence over Req 4.7).
func TestCanAccessSphere_DeactivatedDeniedEvenAdmin(t *testing.T) {
	eng := NewPolicyEngine()

	authzMember := AuthzData{
		Deactivated: true,
		Grants: []GroupGrant{
			{Group: 10, Sphere: 5, Access: data.AccessWrite, Reveal: true},
		},
	}
	got, err := eng.CanAccessSphere(baseCtx(), authzMember, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != data.AccessNone {
		t.Errorf("deactivated member CanAccessSphere = %v, want AccessNone", got)
	}

	authzAdmin := AuthzData{Deactivated: true, Admin: true}
	got, err = eng.CanAccessSphere(baseCtx(), authzAdmin, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != data.AccessNone {
		t.Errorf("deactivated admin CanAccessSphere = %v, want AccessNone", got)
	}
}

// TestCanReveal_AnyGroupWithReveal verifies reveal is granted iff at least one
// granting Group has Reveal on the Sphere (Req 4.4), independent of order and of
// whether other grants lack reveal.
func TestCanReveal_AnyGroupWithReveal(t *testing.T) {
	eng := NewPolicyEngine()
	authz := AuthzData{Grants: []GroupGrant{
		{Group: 10, Sphere: 5, Access: data.AccessRead, Reveal: false},
		{Group: 11, Sphere: 5, Access: data.AccessRead, Reveal: true},
	}}

	if !eng.CanReveal(baseCtx(), authz, 5) {
		t.Error("CanReveal = false, want true (one Group has reveal)")
	}
}

// TestCanReveal_NoGroupWithReveal verifies reveal is withheld when access is
// granted but no granting Group has Reveal (Req 4.4 "otherwise withhold").
func TestCanReveal_NoGroupWithReveal(t *testing.T) {
	eng := NewPolicyEngine()
	authz := AuthzData{Grants: []GroupGrant{
		{Group: 10, Sphere: 5, Access: data.AccessWrite, Reveal: false},
	}}

	if eng.CanReveal(baseCtx(), authz, 5) {
		t.Error("CanReveal = true, want false (no Group has reveal)")
	}
}

// TestCanReveal_PerSphereIsolation verifies reveal on one Sphere does not permit
// reveal on another Sphere (Req 4.4 / 16.6 per-Sphere semantics).
func TestCanReveal_PerSphereIsolation(t *testing.T) {
	eng := NewPolicyEngine()
	authz := AuthzData{Grants: []GroupGrant{
		{Group: 10, Sphere: 5, Access: data.AccessRead, Reveal: true},
		{Group: 10, Sphere: 6, Access: data.AccessRead, Reveal: false},
	}}

	if !eng.CanReveal(baseCtx(), authz, 5) {
		t.Error("CanReveal(sphere 5) = false, want true")
	}
	if eng.CanReveal(baseCtx(), authz, 6) {
		t.Error("CanReveal(sphere 6) = true, want false (reveal must not leak across Spheres)")
	}
}

// TestCanReveal_AdminAlwaysReveals verifies an Admin_Group member may reveal on
// any Sphere without an explicit reveal grant (Req 4.7).
func TestCanReveal_AdminAlwaysReveals(t *testing.T) {
	eng := NewPolicyEngine()
	authz := AuthzData{Admin: true}

	if !eng.CanReveal(baseCtx(), authz, 777) {
		t.Error("admin CanReveal = false, want true")
	}
}

// TestCanReveal_DeactivatedNeverReveals verifies a deactivated user can never
// reveal, even with a reveal grant or admin status (Req 5.4).
func TestCanReveal_DeactivatedNeverReveals(t *testing.T) {
	eng := NewPolicyEngine()

	authzGrant := AuthzData{
		Deactivated: true,
		Grants: []GroupGrant{
			{Group: 10, Sphere: 5, Access: data.AccessRead, Reveal: true},
		},
	}
	if eng.CanReveal(baseCtx(), authzGrant, 5) {
		t.Error("deactivated member CanReveal = true, want false")
	}

	authzAdmin := AuthzData{Deactivated: true, Admin: true}
	if eng.CanReveal(baseCtx(), authzAdmin, 5) {
		t.Error("deactivated admin CanReveal = true, want false")
	}
}

// TestIsAdmin verifies IsAdmin is true only for an active Admin_Group member and
// false for non-admins and deactivated admins (Req 5, 5.4).
func TestIsAdmin(t *testing.T) {
	eng := NewPolicyEngine()

	cases := []struct {
		name  string
		authz AuthzData
		want  bool
	}{
		{"active admin", AuthzData{Admin: true}, true},
		{"non-admin", AuthzData{Admin: false}, false},
		{"deactivated admin", AuthzData{Admin: true, Deactivated: true}, false},
		{"deactivated non-admin", AuthzData{Admin: false, Deactivated: true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eng.IsAdmin(baseCtx(), tc.authz); got != tc.want {
				t.Errorf("IsAdmin = %v, want %v", got, tc.want)
			}
		})
	}
}
