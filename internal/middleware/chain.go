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

package middleware

// This file assembles the ordered request middleware chain and the route-level
// authorization guards handlers hang off it (design.md — "Request /
// Authorization Middleware"). The chain, in order, is:
//
//  1. TLS-enforce  — refuse plaintext when SSL is configured (Req 19.2). Owned
//     by server.PlaintextGuard; the chain simply places it at the head.
//  2. Session      — validate the session cookie token, slide its inactivity
//     window, and reject an expired/unknown token with 401 (Req 3.5, 3.6).
//  3. Tenant-resolve — attach the caller's TenantID (from the validated
//     session's user record, never from client input) so all downstream tenant
//     access is confined to it (Req 1.4).
//  4. Authz        — load the caller's Group memberships and per-Sphere grants,
//     build the immutable RequestContext, and store it plus the raw AuthzData in
//     the request context for the PolicyEngine (Req 4, 5).
//
// The chain establishes identity and authorization state; the actual per-route
// decision (is this an admin route? which Sphere does this route touch?) is made
// by the route-level guards RequireAdmin and RequireSphereAccess, which read the
// state the chain attached. This split matches the design: the chain runs for
// every authenticated route, while admin-vs-Sphere-scoped is a property of the
// route, checked with CanAccessSphere / IsAdmin (design.md — step 4).

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/server"
	"github.com/influence/influence/internal/service"
)

// SessionValidator is the slice of the auth service the Session stage depends
// on: resolving an opaque token to its user and tenant while sliding the
// inactivity window and rejecting expired tokens (service.AuthService satisfies
// it). Declaring it as an interface keeps the chain testable without a full
// auth service and database.
type SessionValidator interface {
	ValidateSession(ctx context.Context, token string) (service.Session, error)
}

// Chain builds the ordered middleware chain from its collaborators. It holds no
// per-request state; Handler produces a fresh http.Handler wrapper for a given
// downstream handler and is safe for concurrent use.
type Chain struct {
	tlsEnabled bool
	sessions   SessionValidator
	loader     AuthzLoader
	policy     service.PolicyEngine
}

// NewChain constructs the middleware chain.
//
//   - tlsEnabled selects whether the TLS-enforce head refuses plaintext (Req
//     19.2); it mirrors the server's TLS configuration.
//   - sessions validates session cookies (Req 3.5, 3.6).
//   - loader loads the caller's authorization inputs from Central + tenant.
//   - policy is the PolicyEngine the route guards evaluate decisions against.
func NewChain(tlsEnabled bool, sessions SessionValidator, loader AuthzLoader, policy service.PolicyEngine) *Chain {
	return &Chain{
		tlsEnabled: tlsEnabled,
		sessions:   sessions,
		loader:     loader,
		policy:     policy,
	}
}

// Handler wraps next with the full ordered chain. The stages are composed
// inside-out so they execute in the documented order at request time:
// TLS-enforce runs first, then Session, then Tenant-resolve + Authz (a single
// stage, since tenant resolution and grant loading both follow directly from
// the validated session).
func (c *Chain) Handler(next http.Handler) http.Handler {
	// Innermost first: the session+tenant+authz stage wraps next, then the TLS
	// guard wraps that, so the guard runs before anything reads the session.
	h := c.sessionTenantAuthz(next)
	h = server.PlaintextGuard(c.tlsEnabled, h)
	return h
}

// Middleware adapts the chain to the chi/`net/http` middleware signature
// (func(http.Handler) http.Handler) so it can be mounted with r.Use.
func (c *Chain) Middleware() func(http.Handler) http.Handler {
	return c.Handler
}

// sessionTenantAuthz is the combined Session → Tenant-resolve → Authz stage. It
// validates the session cookie, resolves the tenant from the session's user
// record, loads the caller's authorization inputs, builds the immutable
// RequestContext, and stores it plus the raw AuthzData in the request context
// for downstream handlers and the route guards.
func (c *Chain) sessionTenantAuthz(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// --- Session (Req 3.5, 3.6) ---
		cookie, err := r.Cookie(service.SessionCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
			return
		}

		sess, err := c.sessions.ValidateSession(r.Context(), cookie.Value)
		if err != nil {
			// Expired, unknown, or otherwise invalid token: deny with 401 and
			// no tenant/user resolved (Req 3.5, 3.6). All validation failures
			// collapse to the same response so an expired session and a forged
			// token are indistinguishable.
			writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
			return
		}

		// --- Tenant-resolve (Req 1.4) ---
		// The tenant comes from the session's user record, never from client
		// input. A provisional RequestContext carrying UserID + TenantID is
		// enough for the loader to reach the caller's own tenant database.
		provisional := data.NewRequestContext(sess.UserID, sess.TenantID, nil, nil)

		// --- Authz (Req 4, 5) ---
		groups, authz, err := c.loader.Load(r.Context(), provisional)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", "authorization load failed")
			return
		}

		// Reduce the per-Group grants to the effective per-Sphere grant map the
		// RequestContext carries for downstream masking decisions (design.md —
		// "attach CanReveal per Sphere"). The PolicyEngine is the single place
		// the union/most-permissive rules live, so the map is computed through
		// it rather than re-implemented here.
		grantsBySphere := effectiveSphereGrants(c.policy, provisional, authz)

		// --- Enrolment gate (Req 33.7, 33.8) ---
		// A 'must_enrol' session passed the password but the tenant requires
		// 2FA and the user is not yet enrolled. Such a session is confined to
		// the enrolment routes; every other route is refused with 403
		// ENROLMENT_REQUIRED. Logout (POST /api/auth/logout) sits OUTSIDE this
		// chain, so it remains reachable without a special allowance here.
		if sess.State == service.SessionStateMustEnrol && !isEnrolmentRoute(r) {
			writeError(w, http.StatusForbidden, "ENROLMENT_REQUIRED", "two-factor enrolment required")
			return
		}

		rc := data.NewRequestContext(sess.UserID, sess.TenantID, groups, grantsBySphere)

		ctx := withRequestContext(r.Context(), rc)
		ctx = withAuthzData(ctx, authz)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// effectiveSphereGrants collapses the raw per-Group grants into the caller's
// effective per-Sphere grant map by evaluating the PolicyEngine for every
// distinct Sphere that appears in a grant. Access is the engine's
// most-permissive union and Reveal is the engine's per-Sphere reveal decision,
// so admin implicit grants and the deactivated override are already applied. A
// Sphere with an effective AccessNone is omitted so the map holds only Spheres
// the caller can actually reach.
func effectiveSphereGrants(policy service.PolicyEngine, rc data.RequestContext, authz service.AuthzData) map[data.SphereID]data.SphereGrant {
	seen := make(map[data.SphereID]struct{}, len(authz.Grants))
	out := make(map[data.SphereID]data.SphereGrant, len(authz.Grants))
	for _, g := range authz.Grants {
		if _, done := seen[g.Sphere]; done {
			continue
		}
		seen[g.Sphere] = struct{}{}

		access, err := policy.CanAccessSphere(rc, authz, g.Sphere)
		if err != nil || access == data.AccessNone {
			continue
		}
		out[g.Sphere] = data.SphereGrant{
			Access: access,
			Reveal: policy.CanReveal(rc, authz, g.Sphere),
		}
	}
	return out
}

// RequireAdmin guards an admin route (design.md — step 4, "for admin routes
// check IsAdmin"). A caller who is not an active Admin_Group member — including
// a deactivated admin (Req 5.4) — is refused with 403 ADMIN_REQUIRED and the
// message "admin privileges required" (Req 5.5); the handler never runs. It
// must be mounted below the chain so the RequestContext and AuthzData are
// present.
func (c *Chain) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, authz, ok := c.authState(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
			return
		}
		if !c.policy.IsAdmin(rc, authz) {
			writeError(w, http.StatusForbidden, "ADMIN_REQUIRED", "admin privileges required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireSphereAccess guards a Sphere-scoped route (design.md — step 4, "for
// Sphere-scoped routes check CanAccessSphere"). The Sphere id is taken from the
// named chi URL parameter and interpreted as the tenant-local integer SphereID.
// A caller lacking at least min access to that Sphere is refused with 403
// NO_SPHERE_ACCESS; the handler never runs.
//
// min is the minimum AccessLevel the route requires: AccessRead for a read
// route, AccessWrite for a mutating route. A deactivated user or a caller with
// no granting Group gets AccessNone from the engine and is therefore denied
// (Req 4.5, 5.4).
func (c *Chain) RequireSphereAccess(param string, min data.AccessLevel) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc, authz, ok := c.authState(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
				return
			}

			sphere, err := sphereIDParam(r, param)
			if err != nil {
				writeError(w, http.StatusBadRequest, "VALIDATION", "invalid sphere identifier")
				return
			}

			access, err := c.policy.CanAccessSphere(rc, authz, sphere)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL", "authorization check failed")
				return
			}
			if access < min {
				writeError(w, http.StatusForbidden, "NO_SPHERE_ACCESS", "no access to this sphere")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authState reads the RequestContext and AuthzData the chain attached. It
// returns ok=false when either is missing, which means the guard was mounted
// without the chain above it — a programming error that fails closed as an
// authentication failure rather than silently allowing the request.
func (c *Chain) authState(r *http.Request) (data.RequestContext, service.AuthzData, bool) {
	rc, ok := RequestContextFrom(r.Context())
	if !ok {
		return data.RequestContext{}, service.AuthzData{}, false
	}
	authz, ok := AuthzDataFrom(r.Context())
	if !ok {
		return data.RequestContext{}, service.AuthzData{}, false
	}
	return rc, authz, true
}

// isEnrolmentRoute reports whether the request targets one of the routes a
// restricted 'must_enrol' session is permitted to reach: the two enrolment
// endpoints (Req 33.7). Logout is deliberately NOT listed here because it is
// mounted outside the chain and is therefore already reachable; matching it
// here would have no effect. The match is on the exact method + path so the
// gate cannot be sidestepped by, e.g., a GET to the enrolment path.
func isEnrolmentRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/api/2fa/enrol", "/api/2fa/confirm":
		return true
	default:
		return false
	}
}

// errNoSphereParam is returned by sphereIDParam when the route parameter is
// absent or not a valid tenant-local Sphere id.
var errNoSphereParam = errors.New("missing or invalid sphere id parameter")

// sphereIDParam reads the named chi URL parameter and parses it as a
// tenant-local integer SphereID. It is deliberately strict: a missing or
// non-integer value is an error the guard maps to a 400 rather than defaulting
// to Sphere 0.
func sphereIDParam(r *http.Request, param string) (data.SphereID, error) {
	raw := chi.URLParam(r, param)
	if raw == "" {
		return 0, errNoSphereParam
	}
	id, err := parseInt64(raw)
	if err != nil {
		return 0, errNoSphereParam
	}
	return data.SphereID(id), nil
}
