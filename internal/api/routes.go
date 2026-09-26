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

package api

// Router construction (design.md — "API Surface"). This file assembles the chi
// router: the unauthenticated login route, the authenticated routes mounted
// behind the middleware chain, the admin routes additionally guarded by
// RequireAdmin, and the WebSocket edit endpoint.
//
// The middleware chain (internal/middleware.Chain) runs for every authenticated
// route: TLS-enforce → Session → Tenant-resolve → Authz. It attaches the
// immutable RequestContext and the raw AuthzData to the request context, which
// the handlers read via middleware.RequestContextFrom / AuthzDataFrom. Route
// guards (RequireAdmin) hang off the same chain for the admin subtree.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	mw "github.com/influence/influence/internal/middleware"
)

// Router builds the complete HTTP handler for the server: a chi router with the
// health check, the unauthenticated auth routes, and every authenticated route
// mounted behind the middleware chain.
//
// The chain is constructed from the server's own collaborators: TLS enforcement
// mirrors cfg.TLS, session validation uses the AuthService, authorization inputs
// are loaded by the database-backed AuthzLoader, and decisions run through the
// shared PolicyEngine. Because the chain's Session stage rejects a request with
// no/invalid session cookie with 401 before any handler runs, every route
// mounted under it is authenticated by construction (Req 3.5, 3.6).
func (s *Server) Router() http.Handler {
	chain := mw.NewChain(s.cfg.TLS, s.auth, mw.NewAuthzLoader(s.conns), s.policy)

	r := chi.NewRouter()

	// Liveness check — unauthenticated, no tenant, no side effects.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// --- Unauthenticated auth routes (login must precede any session) ---
	// Login is the one route that cannot sit behind the Session stage: a caller
	// has no session yet (Req 3.1). Logout also lives here because it operates
	// on the presented cookie directly and must succeed even for a session the
	// Session stage would reject as expired.
	r.Post("/api/auth/login", s.handleLogin)
	r.Post("/api/auth/logout", s.handleLogout)

	// --- Platform documentation (Req 27) ---
	// The in-app docs are general platform documentation, not tenant content,
	// so they sit outside the authenticated chain. The wildcard {path} names a
	// markdown file under the docs directory; the handler confines it there and
	// rejects any path-traversal attempt.
	r.Get("/api/docs/*", s.handleGetDoc)

	// --- First-run setup (Req 31) ---
	// These sit OUTSIDE the middleware chain: while a tenant is in
	// First_Run_State there is no administrator, and therefore no session, to
	// authenticate — the whole point is to create that first admin. The state
	// endpoint reports whether setup is still open; the admin endpoint creates
	// the Bootstrap_Admin in a single Central transaction and, once any admin
	// exists, refuses further calls with SETUP_COMPLETE (Req 31.4, 31.6).
	r.Get("/api/setup/state", s.handleSetupState)
	r.Post("/api/setup/admin", s.handleSetupAdmin)

	// --- Authenticated API routes behind the full middleware chain ---
	r.Group(func(r chi.Router) {
		r.Use(chain.Middleware())

		// Identity (Req 32.1). Available to any authenticated session — behind
		// the chain but NOT behind RequireAdmin — so a non-admin can learn its
		// own identity and 2FA state; the admin flag is reported, not required.
		r.Get("/api/me", s.handleMe)

		// Two-Factor Authentication enrolment (Req 33.1, 33.2, 33.3). Behind
		// the chain but NOT behind RequireAdmin — any authenticated caller
		// enrols themselves via rc.UserID(). Enrol mints a pending TOTP_Secret
		// + provisioning URI; confirm verifies a TOTP and issues Recovery_Codes.
		r.Post("/api/2fa/enrol", s.handleTwoFactorEnrol)
		r.Post("/api/2fa/confirm", s.handleTwoFactorConfirm)

		// Spheres (Req 6, 7, 21).
		r.Get("/api/spheres", s.handleListSpheres)
		r.Post("/api/spheres", s.handleCreateSphere)
		r.Delete("/api/spheres/{id}", s.handleDeleteSphere)
		r.Post("/api/spheres/{id}/circle", s.handleCircle)
		r.Delete("/api/spheres/{id}/circle", s.handleUnCircle)
		r.Put("/api/circles/order", s.handleReorderCircles)

		// Facets (Req 8, 9).
		r.Get("/api/spheres/{id}/facets", s.handleListFacets)
		r.Post("/api/facets", s.handleCreateFacet)
		r.Patch("/api/facets/{id}", s.handleReparentFacet)
		r.Delete("/api/facets/{id}", s.handleDeleteFacet)

		// Polygons (Req 10, 12, 14, 16, 18).
		r.Get("/api/spheres/{id}/polygons", s.handleListPolygons)
		r.Post("/api/polygons", s.handleCreatePolygon)
		r.Get("/api/polygons/{id}", s.handleGetPolygon)
		r.Get("/api/polygons/{id}/render", s.handleRenderPolygon)
		r.Post("/api/polygons/{id}/encrypt", s.handleEncryptSpan)
		r.Post("/api/polygons/{id}/reveal/{tokenId}", s.handleRevealToken)
		r.Post("/api/polygons/{id}/images", s.handleUploadImage)
		r.Get("/api/polygons/{id}/pdf", s.handlePDFExport)

		// Images (Req 17).
		r.Get("/api/images/{imageId}", s.handleServeImage)

		// Search + quick searches (Req 23, 24).
		r.Get("/api/search", s.handleSearch)
		r.Get("/api/quick/created", s.handleQuickCreated)
		r.Get("/api/quick/recent", s.handleQuickRecent)

		// Edit locks (Req 25.3, 25.5).
		r.Post("/api/polygons/{id}/lock", s.handleAcquireLock)
		r.Delete("/api/polygons/{id}/lock", s.handleReleaseLock)
		r.Post("/api/polygons/{id}/lock/heartbeat", s.handleHeartbeatLock)

		// Bookmarks (Req 26).
		r.Get("/api/bookmarks", s.handleListBookmarks)
		r.Post("/api/bookmarks", s.handleAddBookmark)
		r.Delete("/api/bookmarks/{polygonId}", s.handleRemoveBookmark)

		// WebSocket collaborative edits / lock signaling (Req 25.2).
		r.Get("/ws/polygons/{id}", s.handleWebSocket)

		// Admin routes: same chain, plus the admin guard (Req 5, 1.7, 2).
		r.Group(func(r chi.Router) {
			r.Use(chain.RequireAdmin)
			r.Post("/api/admin/tenants", s.handleCreateTenant)
			r.Get("/api/admin/tenants/{id}/export", s.handleExportTenant)

			// Tenant Two-Factor_Authentication policy toggle (Req 33.7, 33.8).
			// Sets the caller's tenant policy optional/required; tenant-scoped
			// from the RequestContext, so an admin only ever toggles their own
			// tenant's policy.
			r.Put("/api/admin/2fa-policy", s.handleSetTwoFactorPolicy)

			// Admin user lifecycle (Req 32.2, 5.1, 5.4). List/create users in
			// the caller's tenant and toggle a user's deactivated flag; all
			// tenant-scoped from the RequestContext.
			r.Get("/api/admin/users", s.handleListUsers)
			r.Post("/api/admin/users", s.handleCreateUser)
			r.Post("/api/admin/users/{id}/deactivate", s.handleDeactivateUser)
			r.Post("/api/admin/users/{id}/reactivate", s.handleReactivateUser)

			// Admin Group + membership management (Req 5.2, 32.3). List/create
			// Groups in the caller's tenant and add/remove memberships; all
			// tenant-scoped from the RequestContext, and a removal that would
			// leave a user with zero Groups is a VALIDATION error (Req 4.1).
			r.Get("/api/admin/groups", s.handleListGroups)
			r.Post("/api/admin/groups", s.handleCreateGroup)
			r.Post("/api/admin/groups/{id}/members", s.handleAddGroupMember)
			r.Delete("/api/admin/groups/{id}/members/{userId}", s.handleRemoveGroupMember)

			// Admin per-Sphere Group access matrix (Req 5.3, 32.4). List the
			// grants in the caller's tenant, grant a Group access (level +
			// reveal) to a Sphere, and revoke it; all tenant-scoped from the
			// RequestContext, and a Sphere/Group outside the tenant is rejected
			// with TENANT_ISOLATION (Req 32.6).
			r.Get("/api/admin/sphere-access", s.handleListSphereAccess)
			r.Put("/api/admin/sphere-access", s.handleGrantSphereAccess)
			r.Delete("/api/admin/sphere-access", s.handleRevokeSphereAccess)
		})
	})

	return r
}
