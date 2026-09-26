// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package service

// The PolicyEngine centralizes every per-Sphere authorization decision
// (design.md — "Authorization Middleware — Req 4, 5"). It is the single place
// that answers three questions the rest of the backend defers to:
//
//   - CanAccessSphere: what access (none/read/write) does the caller have to a
//     Sphere, taking the most-permissive union across the caller's Groups
//     (Requirement 4.3)? A caller in no Group with access to the Sphere gets
//     AccessNone (Requirement 4.5).
//   - CanReveal: may the caller reveal Sensitive_Data in a Sphere — true iff
//     any granting Group has Reveal on that Sphere (Requirement 4.4), evaluated
//     per-Sphere.
//   - IsAdmin: is the caller a member of the Admin_Group, which unlocks the
//     admin pages (Requirement 5) and implies write+reveal on every Sphere in
//     the tenant (Requirement 4.7)?
//
// Two cross-cutting rules the engine enforces uniformly:
//
//   - Admin_Group membership yields an implicit grant of write + reveal on
//     EVERY Sphere in the caller's tenant, overriding the per-Group grant map
//     (Requirement 4.7). An admin therefore never needs an explicit SPHERE_GRANT
//     row to reach a Sphere.
//   - A deactivated user is denied ALL Spheres and ALL admin pages, while the
//     account and its Group memberships are retained (Requirement 5.4). The
//     deactivated check fails closed and takes precedence over every grant,
//     including the Admin_Group implicit grant — a deactivated admin is still
//     locked out.
//
// Where the inputs come from. The middleware chain (task 6.2) loads the
// caller's Group memberships and, from those Groups, the per-Sphere SPHERE_GRANT
// rows for the caller's tenant, plus the Admin_Group and deactivated flags from
// the Central_Directory. It packages them into an AuthzData value and evaluates
// decisions through this engine. The engine performs the union itself rather
// than trusting a pre-reduced map, so the "most-permissive across Groups" rule
// (Req 4.3) and the "reveal iff any Group has reveal" rule (Req 4.4) live in one
// audited place. The engine holds no database handle and does no I/O: it is a
// pure function of the authorization data the middleware already loaded, which
// keeps every per-request access check allocation-free and trivially testable.

import "github.com/influence/influence/internal/data"

// GroupGrant is a single Group's grant on a single Sphere, as stored in the
// tenant's SPHERE_GRANT table (sphere_id, group_id, access, reveal). The
// PolicyEngine receives the caller's grants as the set of GroupGrants for the
// Groups the caller belongs to and reduces them to an effective decision. Group
// is retained so the union is computed from the raw per-Group rows (Req 4.3)
// rather than a map that has already collapsed the Groups together.
type GroupGrant struct {
	Group  data.GroupID
	Sphere data.SphereID
	Access data.AccessLevel
	Reveal bool
}

// AuthzData is the raw authorization input the middleware loads for a request:
// the caller's Admin_Group membership, deactivation status, and the full set of
// per-Group Sphere grants for the caller's tenant. The PolicyEngine reduces this
// to effective decisions. It is deliberately a plain data value — the middleware
// builds it from the Central_Directory (admin/deactivated) and the tenant's
// SPHERE_GRANT rows (grants), and the engine never reads a database itself.
type AuthzData struct {
	// Admin reports whether the caller is a member of the Admin_Group. An admin
	// gets an implicit write+reveal grant on every Sphere in the tenant
	// (Req 4.7) and may reach the admin pages (Req 5).
	Admin bool
	// Deactivated reports whether the caller's account has been deactivated. A
	// deactivated user is denied all Spheres and admin pages while retaining the
	// account and memberships (Req 5.4). This flag fails closed and overrides
	// every grant, including the admin implicit grant.
	Deactivated bool
	// Grants is the caller's per-Group Sphere grants across all the Groups the
	// caller belongs to. Multiple entries may reference the same Sphere via
	// different Groups; the engine takes the most-permissive union.
	Grants []GroupGrant
}

// PolicyEngine answers the centralized authorization questions. It is the
// interface the middleware and downstream services depend on so the access
// rules live behind one type (design.md — "Authorization decisions are
// centralized in a PolicyEngine"). All methods take the caller's RequestContext
// (identity) and the AuthzData the middleware loaded for that request.
type PolicyEngine interface {
	// CanAccessSphere returns the caller's effective access to a Sphere: the
	// most-permissive level across the caller's Groups, or write for an admin,
	// or AccessNone for a deactivated user or a caller with no granting Group
	// (Req 4.3, 4.5, 4.7, 5.4).
	CanAccessSphere(ctx data.RequestContext, authz AuthzData, sphere data.SphereID) (data.AccessLevel, error)
	// CanReveal reports whether the caller may reveal Sensitive_Data in a
	// Sphere — true iff any granting Group has Reveal, or the caller is an
	// active admin; always false for a deactivated user (Req 4.4, 4.7, 5.4).
	CanReveal(ctx data.RequestContext, authz AuthzData, sphere data.SphereID) bool
	// IsAdmin reports whether the caller may reach the admin pages: an
	// Admin_Group member who is not deactivated (Req 5, 5.4).
	IsAdmin(ctx data.RequestContext, authz AuthzData) bool
}

// policyEngine is the concrete, stateless PolicyEngine. It holds no fields: every
// decision is a pure function of the RequestContext and AuthzData passed in, so a
// single instance is safe for concurrent use across all requests.
type policyEngine struct{}

// NewPolicyEngine returns the default PolicyEngine. The returned value is
// stateless and safe to share across goroutines.
func NewPolicyEngine() PolicyEngine {
	return policyEngine{}
}

// CanAccessSphere computes the caller's effective access to sphere.
//
// Precedence, highest first:
//  1. Deactivated → AccessNone. A deactivated user reaches no Sphere regardless
//     of any grant or admin status (Req 5.4); this check fails closed.
//  2. Admin → AccessWrite. An Admin_Group member has an implicit write grant on
//     every Sphere in the tenant (Req 4.7), so no explicit grant is consulted.
//  3. Otherwise the most-permissive Access across the caller's Groups that hold
//     a grant on this Sphere (Req 4.3). A caller with no granting Group gets
//     AccessNone, which the middleware maps to a "lacks access" denial (Req 4.5).
//
// It never returns an error today; the error return is part of the design's
// interface signature so a future implementation that must consult external
// state can surface a failure without a breaking change.
func (policyEngine) CanAccessSphere(_ data.RequestContext, authz AuthzData, sphere data.SphereID) (data.AccessLevel, error) {
	if authz.Deactivated {
		return data.AccessNone, nil
	}
	if authz.Admin {
		return data.AccessWrite, nil
	}

	effective := data.AccessNone
	for _, g := range authz.Grants {
		if g.Sphere != sphere {
			continue
		}
		if g.Access > effective {
			effective = g.Access
		}
	}
	return effective, nil
}

// CanReveal reports whether the caller may reveal Sensitive_Data in sphere.
//
// Precedence mirrors CanAccessSphere:
//  1. Deactivated → false, unconditionally (Req 5.4).
//  2. Admin → true. The Admin_Group implicit grant includes reveal on every
//     Sphere (Req 4.7).
//  3. Otherwise true iff any of the caller's Groups holds a grant on this Sphere
//     with Reveal set (Req 4.4). Reveal is per-Sphere: a Group's reveal on one
//     Sphere never leaks to another because only grants for this sphere are
//     considered.
func (policyEngine) CanReveal(_ data.RequestContext, authz AuthzData, sphere data.SphereID) bool {
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

// IsAdmin reports whether the caller may reach the admin pages: an Admin_Group
// member (Req 5) who has not been deactivated (Req 5.4). A deactivated admin is
// denied the admin pages while retaining the account and memberships, so the
// deactivated check takes precedence.
func (policyEngine) IsAdmin(_ data.RequestContext, authz AuthzData) bool {
	return authz.Admin && !authz.Deactivated
}
