package service

// Feature: influence, Property 3: Effective per-Sphere access
//
// Property 3 (design.md — Correctness Properties): "For all users, Groups,
// grants, and Spheres, a user's effective access to a Sphere equals the
// most-permissive access across the user's Groups that grant it (with Reveal
// true iff any such Group has Reveal), access is denied exactly when no Group
// grants it, and Admin_Group members have write+reveal on every Sphere in their
// tenant."
//
// Validates: Requirements 4.3, 4.5, 4.7
//
// This test drives the PolicyEngine across randomized authorization inputs and
// checks it against an independent reference model that re-derives effective
// access straight from the property's plain-language statement. Because the
// reference is written from the requirement rather than from policy.go, the two
// agreeing is evidence the engine implements the requirement, not merely that it
// is self-consistent. The rapid default of 100 iterations satisfies the
// "minimum 100 iterations" requirement; each iteration draws a fresh universe of
// Groups, grants, and Spheres.

import (
	"testing"

	"github.com/influence/influence/internal/data"
	"pgregory.net/rapid"
)

// referenceAccess re-derives the effective access for a Sphere directly from the
// property statement, independently of policyEngine. Precedence: deactivated
// denies everything (Req 5.4 overrides), an admin has write on every Sphere
// (Req 4.7), otherwise the most-permissive Access across the Groups that grant
// this Sphere, or AccessNone when none does (Req 4.3, 4.5).
func referenceAccess(authz AuthzData, sphere data.SphereID) data.AccessLevel {
	if authz.Deactivated {
		return data.AccessNone
	}
	if authz.Admin {
		return data.AccessWrite
	}
	best := data.AccessNone
	for _, g := range authz.Grants {
		if g.Sphere != sphere {
			continue
		}
		if g.Access > best {
			best = g.Access
		}
	}
	return best
}

// referenceReveal re-derives reveal for a Sphere from the property statement:
// deactivated never reveals, an admin always reveals (Req 4.7), otherwise reveal
// iff at least one granting Group on this Sphere has Reveal (Req 4.3 union).
func referenceReveal(authz AuthzData, sphere data.SphereID) bool {
	if authz.Deactivated {
		return false
	}
	if authz.Admin {
		return true
	}
	for _, g := range authz.Grants {
		if g.Sphere == sphere && g.Reveal {
			return true
		}
	}
	return false
}

// TestEffectivePerSphereAccessProperty is Property 3: for randomly generated
// callers, Groups, grants, and Spheres, the PolicyEngine's per-Sphere access and
// reveal decisions match the reference model derived from Requirements 4.3, 4.5,
// and 4.7 (with the 5.4 deactivation override).
func TestEffectivePerSphereAccessProperty(t *testing.T) {
	eng := NewPolicyEngine()

	// A small, fixed universe keeps overlap between grants likely (so the
	// most-permissive union is genuinely exercised) while still ranging over
	// every combination of access levels, reveal flags, admin, and deactivation.
	const maxSphere = 6

	accessGen := rapid.SampledFrom([]data.AccessLevel{
		data.AccessNone, data.AccessRead, data.AccessWrite,
	})

	rapid.Check(t, func(rt *rapid.T) {
		admin := rapid.Bool().Draw(rt, "admin")
		deactivated := rapid.Bool().Draw(rt, "deactivated")

		// Draw a set of per-Group Sphere grants. Multiple grants may target the
		// same Sphere via different Groups, which is exactly the overlap the
		// union rule (Req 4.3) must resolve most-permissively.
		nGrants := rapid.IntRange(0, 12).Draw(rt, "nGrants")
		grants := make([]GroupGrant, nGrants)
		for i := 0; i < nGrants; i++ {
			grants[i] = GroupGrant{
				Group:  data.GroupID(rapid.IntRange(1, 4).Draw(rt, "group")),
				Sphere: data.SphereID(rapid.IntRange(1, maxSphere).Draw(rt, "grantSphere")),
				Access: accessGen.Draw(rt, "access"),
				Reveal: rapid.Bool().Draw(rt, "reveal"),
			}
		}

		authz := AuthzData{Admin: admin, Deactivated: deactivated, Grants: grants}
		ctx := data.NewRequestContext(1, 1, []data.GroupID{1, 2, 3, 4}, nil)

		// Check every Sphere in the universe, including Spheres no grant
		// mentions (which must resolve to AccessNone for a non-admin — Req 4.5).
		for s := 1; s <= maxSphere; s++ {
			sphere := data.SphereID(s)

			gotAccess, err := eng.CanAccessSphere(ctx, authz, sphere)
			if err != nil {
				rt.Fatalf("CanAccessSphere returned error: %v", err)
			}
			wantAccess := referenceAccess(authz, sphere)
			if gotAccess != wantAccess {
				rt.Fatalf("CanAccessSphere(sphere=%d) = %v, want %v (admin=%v deactivated=%v grants=%+v)",
					s, gotAccess, wantAccess, admin, deactivated, grants)
			}

			gotReveal := eng.CanReveal(ctx, authz, sphere)
			wantReveal := referenceReveal(authz, sphere)
			if gotReveal != wantReveal {
				rt.Fatalf("CanReveal(sphere=%d) = %v, want %v (admin=%v deactivated=%v grants=%+v)",
					s, gotReveal, wantReveal, admin, deactivated, grants)
			}

			// Cross-checks stated directly in the property, independent of the
			// reference model:

			// Req 4.5: access is denied EXACTLY when no Group grants it — for a
			// non-admin, non-deactivated caller.
			if !admin && !deactivated {
				granted := false
				for _, g := range grants {
					if g.Sphere == sphere && g.Access > data.AccessNone {
						granted = true
						break
					}
				}
				if granted == (gotAccess == data.AccessNone) {
					rt.Fatalf("Req 4.5 mismatch on sphere=%d: granted=%v but access=%v", s, granted, gotAccess)
				}
			}

			// Req 4.7: an active Admin_Group member has write+reveal on EVERY
			// Sphere, regardless of grants.
			if admin && !deactivated {
				if gotAccess != data.AccessWrite {
					rt.Fatalf("Req 4.7: admin access on sphere=%d = %v, want AccessWrite", s, gotAccess)
				}
				if !gotReveal {
					rt.Fatalf("Req 4.7: admin reveal on sphere=%d = false, want true", s)
				}
			}
		}
	})
}
