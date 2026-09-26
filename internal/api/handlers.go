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

// Thin REST handlers (design.md — "API handlers. Thin; they parse input, call a
// service, and shape the response. No business rules live here.").
//
// Each handler follows the same shape: read the immutable RequestContext (and,
// where a per-Sphere/reveal decision is needed, the AuthzData) the middleware
// chain attached; parse the request body / URL params; call the relevant
// service or repository with that context; and shape the response or map the
// error through writeServiceError. Tenant scoping is structural — the service
// layer resolves the tenant from the RequestContext, never from client input —
// so handlers never touch a tenant id.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/influence/influence/internal/data"
	mw "github.com/influence/influence/internal/middleware"
	"github.com/influence/influence/internal/recordid"
	"github.com/influence/influence/internal/repo"
	"github.com/influence/influence/internal/service"
	"github.com/influence/influence/internal/ws"
)

// maxBodyBytes bounds a JSON request body so a handler cannot be forced to
// buffer an unbounded payload. Image uploads use their own larger limit.
const maxBodyBytes = 1 << 20 // 1 MiB

// writeJSON encodes v as the response body with a 200 (or the given status) and
// the JSON content type. A nil v writes just the status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// requestContext reads the RequestContext the middleware chain attached. Its
// absence means the handler was mounted outside the authenticated chain — a
// wiring bug — so it writes 401 and reports ok=false rather than proceeding
// with a zero context.
func requestContext(w http.ResponseWriter, r *http.Request) (data.RequestContext, bool) {
	rc, ok := mw.RequestContextFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "authentication required")
		return data.RequestContext{}, false
	}
	return rc, true
}

// authzData reads both the RequestContext and the raw AuthzData the chain
// attached, needed by handlers that make a per-Sphere / reveal decision through
// the PolicyEngine (reveal, PDF export). Absence is treated as an auth failure.
func authzData(w http.ResponseWriter, r *http.Request) (data.RequestContext, service.AuthzData, bool) {
	rc, ok := mw.RequestContextFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "authentication required")
		return data.RequestContext{}, service.AuthzData{}, false
	}
	authz, ok := mw.AuthzDataFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "authentication required")
		return data.RequestContext{}, service.AuthzData{}, false
	}
	return rc, authz, true
}

// decodeJSON reads and decodes a JSON body into dst with the shared size bound.
// A malformed or oversize body is a 400 BAD_REQUEST; the handler stops on
// ok=false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid request body")
		return false
	}
	return true
}

// -----------------------------------------------------------------------------
// Auth (Req 3)
// -----------------------------------------------------------------------------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Code is the optional second factor: a TOTP or a single-use Recovery_Code
	// (Req 33.4, 33.6). It is omitted on the first submission; when the account
	// is 2FA-enrolled the server responds 202 "second factor required" and the
	// client re-submits with the code populated.
	Code string `json:"code,omitempty"`
}

// handleLogin authenticates a user and, on success, sets the session cookie
// (Req 3.1–3.4, 33.4–33.6). Invalid credentials, a locked account, or a
// deactivated account all return 401 with a generic message so the specific
// reason is not disclosed (Req 3.2); a wrong second factor is likewise a
// generic 401 so which factor failed is not leaked (Req 33.5).
//
// The handler distinguishes three positive outcomes:
//
//   - A fully authenticated login (no 2FA owed, or a valid second factor
//     supplied) sets the session cookie and returns 200 with an 'active'
//     session.
//   - An enrolled user who supplied no code gets 202 "second factor required"
//     and NO cookie: the client must re-POST username+password+code.
//   - An unenrolled user in a tenant whose policy requires 2FA gets a restricted
//     'must_enrol' session cookie and 202 "enrolment required"; the middleware
//     confines that session to the enrolment + logout routes until they enrol
//     (Req 33.7).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	sess, err := s.auth.LoginWithFactor(r.Context(), req.Username, req.Password, req.Code)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrSecondFactorRequired):
			// Password correct, 2FA enrolled, no code yet: prompt for the
			// second factor without establishing any session (Req 33.4).
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "second_factor_required"})
		case errors.Is(err, service.ErrInvalidCredentials),
			errors.Is(err, service.ErrAccountLocked),
			errors.Is(err, service.ErrAccountDeactivated):
			writeError(w, http.StatusUnauthorized, codeUnauthenticated, "authentication failed")
		default:
			writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		}
		return
	}

	// A restricted 'must_enrol' session is a real session (the middleware gates
	// it to enrolment + logout), so its cookie is set, but the status is 202
	// "enrolment required" so the client routes the user straight to enrolment
	// rather than the app (Req 33.7).
	if sess.State == service.SessionStateMustEnrol {
		http.SetCookie(w, service.SessionCookie(sess.Token))
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "enrolment_required"})
		return
	}

	http.SetCookie(w, service.SessionCookie(sess.Token))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLogout terminates the presented session and clears the cookie (Req
// 3.5). It operates on the cookie directly rather than behind the Session
// stage, so an already-expired session still logs out cleanly. A missing cookie
// is a no-op success — the desired end state (no session) already holds.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(service.SessionCookieName); err == nil && cookie.Value != "" {
		if err := s.auth.Logout(r.Context(), cookie.Value); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
			return
		}
	}
	http.SetCookie(w, service.ClearSessionCookie())
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// -----------------------------------------------------------------------------
// First-run setup (Req 31)
// -----------------------------------------------------------------------------

// setupRequest is the shared body of both setup endpoints. Tenant is an optional
// tenant_uuid used to disambiguate a multi-tenant host; on the common
// single-tenant host it is omitted and the sole active tenant is resolved. The
// state endpoint reads only Tenant; the admin endpoint also reads the
// credentials. Because GET carries no body, the state endpoint takes Tenant from
// the "tenant" query parameter instead (see handleSetupState).
type setupAdminRequest struct {
	Tenant   string `json:"tenant,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// setupStateResponse reports whether the tenant is still in First_Run_State —
// i.e. whether the Setup_Screen should be shown in place of login (Req 31.1).
type setupStateResponse struct {
	FirstRun bool `json:"firstRun"`
}

// handleSetupState reports whether the tenant still needs a Bootstrap_Admin
// (Req 31.1). It is unauthenticated — there is no admin yet to authenticate as —
// and sits outside the middleware chain. The tenant is resolved from the
// optional "tenant" query parameter (a tenant_uuid); when omitted the sole
// active tenant on the host is used. A tenant that cannot be resolved is a 404
// NOT_FOUND. When no tenant can be resolved because setup does not apply, the
// endpoint reports the mapping failure rather than guessing a state.
func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.bootstrap.ResolveActiveTenant(r.Context(), r.URL.Query().Get("tenant"))
	if err != nil {
		if errors.Is(err, service.ErrTenantNotFound) {
			writeError(w, http.StatusNotFound, codeNotFound, "tenant not found")
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	needed, err := s.bootstrap.FirstRunState(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, setupStateResponse{FirstRun: needed})
}

// handleSetupAdmin creates the Bootstrap_Admin for the tenant (Req 31.2). It is
// unauthenticated and delegates the whole operation — the in-transaction
// First_Run_State re-check, validation, hashing, and the USER + Admin_Group +
// membership inserts — to the BootstrapService. Once any admin exists the
// service returns ErrSetupComplete, which writeServiceError maps to 409
// SETUP_COMPLETE (Req 31.4, 31.6); a weak password or invalid username maps to
// 400 VALIDATION (Req 31.5). The response echoes the created user id and reports
// firstRun=false so the client can flip straight to the login screen (Req 31.3).
func (s *Server) handleSetupAdmin(w http.ResponseWriter, r *http.Request) {
	var req setupAdminRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, err := s.bootstrap.ResolveActiveTenant(r.Context(), req.Tenant)
	if err != nil {
		if errors.Is(err, service.ErrTenantNotFound) {
			writeError(w, http.StatusNotFound, codeNotFound, "tenant not found")
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	userID, err := s.bootstrap.CreateBootstrapAdmin(r.Context(), tenant, req.Username, req.Password)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"userId":   int64(userID),
		"firstRun": false,
	})
}

// -----------------------------------------------------------------------------
// Spheres (Req 6, 7, 21)
// -----------------------------------------------------------------------------

type sphereResponse struct {
	RecordID  string `json:"recordId"`
	ShortID   string `json:"shortId"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
}

func toSphereResponse(sp repo.Sphere) sphereResponse {
	return sphereResponse{
		RecordID:  sp.RecordID.Canonical(),
		ShortID:   sp.RecordID.Short(),
		Name:      sp.Name,
		CreatedAt: sp.CreatedAt,
	}
}

// handleListSpheres lists the caller's accessible Spheres with the circled ones
// first in the user's order, then the alphabetical remainder (Req 7, 21).
func (s *Server) handleListSpheres(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	spheres, err := s.circles.ListOrdered(r.Context(), rc)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]sphereResponse, 0, len(spheres))
	for _, sp := range spheres {
		out = append(out, toSphereResponse(sp))
	}
	writeJSON(w, http.StatusOK, map[string]any{"spheres": out})
}

type createSphereRequest struct {
	Name string `json:"name"`
}

// handleCreateSphere creates a Sphere (Req 6.2, 6.4). Name validation lives in
// the repo; a rejected name maps to a VALIDATION envelope.
func (s *Server) handleCreateSphere(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req createSphereRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sp, err := s.spheres.Create(r.Context(), rc, req.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSphereResponse(sp))
}

// handleDeleteSphere deletes a Sphere and its contents (Req 6.5). The {id} URL
// param is the Sphere Record_ID (short or canonical).
func (s *Server) handleDeleteSphere(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	id, ok := parseRecordIDParam(w, r, "id")
	if !ok {
		return
	}
	if err := s.spheres.Delete(r.Context(), rc, id); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// handleCircle circles a Sphere for the caller (Req 7.1). Idempotent per the
// repo.
func (s *Server) handleCircle(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	id, ok := parseRecordIDParam(w, r, "id")
	if !ok {
		return
	}
	if err := s.circles.Circle(r.Context(), rc, id); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleUnCircle un-circles a Sphere for the caller (Req 7.6).
func (s *Server) handleUnCircle(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	id, ok := parseRecordIDParam(w, r, "id")
	if !ok {
		return
	}
	if err := s.circles.UnCircle(r.Context(), rc, id); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type reorderCirclesRequest struct {
	Order []string `json:"order"`
}

// handleReorderCircles reorders the caller's circled Sphere list (Req 7.4).
func (s *Server) handleReorderCircles(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req reorderCirclesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ids := make([]recordid.ID, 0, len(req.Order))
	for _, raw := range req.Order {
		id, err := parseRecordID(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeValidation, "invalid sphere identifier")
			return
		}
		ids = append(ids, id)
	}
	if err := s.circles.Reorder(r.Context(), rc, ids); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// -----------------------------------------------------------------------------
// Facets (Req 8, 9)
// -----------------------------------------------------------------------------

type facetResponse struct {
	RecordID       string  `json:"recordId"`
	SphereRecordID string  `json:"sphereRecordId"`
	ParentRecordID *string `json:"parentRecordId,omitempty"`
	Name           string  `json:"name"`
}

func toFacetResponse(f repo.Facet) facetResponse {
	return facetResponse{
		RecordID:       f.RecordID,
		SphereRecordID: f.SphereRecordID,
		ParentRecordID: f.ParentRecordID,
		Name:           f.Name,
	}
}

// handleListFacets lists the Facets in a Sphere (Req 8). The {id} param is the
// Sphere Record_ID.
func (s *Server) handleListFacets(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	sphereID := chi.URLParam(r, "id")
	facets, err := s.facets.List(r.Context(), rc, sphereID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]facetResponse, 0, len(facets))
	for _, f := range facets {
		out = append(out, toFacetResponse(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"facets": out})
}

type createFacetRequest struct {
	SphereRecordID string  `json:"sphereRecordId"`
	ParentRecordID *string `json:"parentRecordId"`
	Name           string  `json:"name"`
}

// handleCreateFacet creates a Facet under a Sphere and optional parent Facet
// (Req 8, 9.3).
func (s *Server) handleCreateFacet(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req createFacetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	f, err := s.facets.Create(r.Context(), rc, req.SphereRecordID, req.ParentRecordID, req.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toFacetResponse(f))
}

type reparentFacetRequest struct {
	ParentRecordID *string `json:"parentRecordId"`
}

// handleReparentFacet moves a Facet under a new parent (or to top level),
// enforcing hierarchy integrity (Req 9.1, 9.3). The {id} param is the Facet
// Record_ID.
func (s *Server) handleReparentFacet(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req reparentFacetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	facetID := chi.URLParam(r, "id")
	if err := s.facets.Reparent(r.Context(), rc, facetID, req.ParentRecordID); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDeleteFacet deletes a Facet and its descendants/contained Polygons (Req
// 8.6).
func (s *Server) handleDeleteFacet(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	facetID := chi.URLParam(r, "id")
	if err := s.facets.Delete(r.Context(), rc, facetID); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// -----------------------------------------------------------------------------
// Polygons (Req 10, 12, 16, 18)
// -----------------------------------------------------------------------------

type polygonResponse struct {
	RecordID       string  `json:"recordId"`
	SphereRecordID string  `json:"sphereRecordId"`
	FacetRecordID  *string `json:"facetRecordId,omitempty"`
	ParentRecordID *string `json:"parentRecordId,omitempty"`
	Type           string  `json:"type"`
	Content        string  `json:"content"`
	EditMode       string  `json:"editMode"`
	AuthorUserID   int64   `json:"authorUserId"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
}

func toPolygonResponse(p repo.Polygon) polygonResponse {
	return polygonResponse{
		RecordID:       p.RecordID,
		SphereRecordID: p.SphereRecordID,
		FacetRecordID:  p.FacetRecordID,
		ParentRecordID: p.ParentRecordID,
		Type:           string(p.Type),
		Content:        p.Content,
		EditMode:       string(p.EditMode),
		AuthorUserID:   p.AuthorUserID,
		CreatedAt:      p.CreatedAt,
		UpdatedAt:      p.UpdatedAt,
	}
}

// handleListPolygons lists the Polygons within a Sphere (Req 10). The {id}
// param is the Sphere Record_ID.
func (s *Server) handleListPolygons(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	sphereID := chi.URLParam(r, "id")
	polys, err := s.polygons.List(r.Context(), rc, sphereID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]polygonResponse, 0, len(polys))
	for _, p := range polys {
		out = append(out, toPolygonResponse(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"polygons": out})
}

type createPolygonRequest struct {
	SphereRecordID string  `json:"sphereRecordId"`
	Type           string  `json:"type"`
	FacetRecordID  *string `json:"facetRecordId"`
	ParentRecordID *string `json:"parentRecordId"`
}

// handleCreatePolygon creates a Polygon of the given type inside a Sphere (Req
// 10.1–10.7). The author is the authenticated user from the RequestContext.
func (s *Server) handleCreatePolygon(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req createPolygonRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.polygons.Create(r.Context(), rc, req.SphereRecordID, repo.PolygonType(req.Type),
		int64(rc.UserID()), repo.CreateOptions{FacetRecordID: req.FacetRecordID, ParentRecordID: req.ParentRecordID})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPolygonResponse(p))
}

// handleGetPolygon returns a Polygon's stored state (Req 10). It also records a
// view so "Recently Viewed" reflects the access (Req 24.2); a view-log failure
// does not fail the read.
func (s *Server) handleGetPolygon(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	p, err := s.polygons.Get(r.Context(), rc, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	_ = s.quick.RecordView(r.Context(), rc, id)
	writeJSON(w, http.StatusOK, toPolygonResponse(p))
}

type renderResponse struct {
	HTML   string                  `json:"html"`
	Footer *service.MetadataFooter `json:"footer,omitempty"`
}

// handleRenderPolygon renders a Polygon to masked HTML for the caller's context
// (Req 12, 14, 16.3, 18). Cross-Refraction links resolve to current names, every
// Obfuscation_Token is masked (no per-token reveal on this route), and the
// metadata footer is appended.
func (s *Server) handleRenderPolygon(w http.ResponseWriter, r *http.Request) {
	rc, authz, ok := authzData(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	p, err := s.polygons.Get(r.Context(), rc, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	// Resolve Cross-Refraction links for this viewer, then mask every token
	// (reveal is off for the whole-page render — per-token reveal is its own
	// route), then render markdown → HTML.
	resolved, err := s.links.ResolveContent(r.Context(), rc, authz, p.Content)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	masked := service.Mask(resolved, false, nil)
	htmlBytes, err := markdownRender(masked)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "render failed")
		return
	}

	footer, err := s.footer.Build(r.Context(), p.AuthorUserID, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "footer render failed")
		return
	}

	_ = s.quick.RecordView(r.Context(), rc, id)
	writeJSON(w, http.StatusOK, renderResponse{HTML: string(htmlBytes), Footer: &footer})
}

type encryptSpanRequest struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// handleEncryptSpan encrypts a span of a Polygon's content, replacing it with an
// Obfuscation_Token (Req 16.1, 16.2). Returns the minted token id and the
// updated content (which contains no plaintext).
func (s *Server) handleEncryptSpan(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req encryptSpanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	res, err := s.encryption.EncryptSpan(r.Context(), rc, id, req.Start, req.End)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"tokenId":    res.TokenID,
		"newContent": res.NewContent,
	})
}

// handleRevealToken reveals a single Obfuscation_Token's plaintext for a caller
// holding Reveal_Permission for the token's owning Sphere (Req 16.4, 16.5,
// 16.8). Denied and failed cases carry no plaintext and map to distinct
// envelopes.
func (s *Server) handleRevealToken(w http.ResponseWriter, r *http.Request) {
	rc, authz, ok := authzData(w, r)
	if !ok {
		return
	}
	tokenID := chi.URLParam(r, "tokenId")
	plaintext, err := s.encryption.RevealToken(r.Context(), rc, s.policy, authz, tokenID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"plaintext": plaintext})
}

// -----------------------------------------------------------------------------
// Images (Req 17)
// -----------------------------------------------------------------------------

// handleUploadImage stores a pasted image blob and returns the in-content
// reference to insert (Req 17.1–17.3). The raw bytes are the request body;
// format and size are validated by the repo against the sniffed content type.
func (s *Server) handleUploadImage(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	// The Polygon id is part of the route for context (Req 17 pastes into a
	// Polygon), but the blob is tenant-scoped; validate the id is a Record_ID.
	if _, ok := parseRecordIDParam(w, r, "id"); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, repo.MaxImageBytes+1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "image exceeds maximum allowed size")
		return
	}
	ref, err := s.images.StoreImage(r.Context(), rc, body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"reference": ref})
}

// handleServeImage streams a stored image blob (Req 17.4). An image not in the
// caller's tenant is not accessible (Req 1.6).
func (s *Server) handleServeImage(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	imageID := chi.URLParam(r, "imageId")
	img, err := s.images.GetImage(r.Context(), rc, imageID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", img.MIME)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img.Data)
}

// -----------------------------------------------------------------------------
// PDF export (Req 22)
// -----------------------------------------------------------------------------

// handlePDFExport renders a Polygon to PDF for the caller's context (Req 22).
// Masking is inherited from the render path. A missing renderer (default build)
// surfaces as an internal error so no empty file is emitted.
func (s *Server) handlePDFExport(w http.ResponseWriter, r *http.Request) {
	rc, authz, ok := authzData(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	pdfBytes, err := s.pdf.PDFExport(r.Context(), rc, authz, id)
	if err != nil {
		if errors.Is(err, service.ErrPolygonNotAccessible) {
			writeServiceError(w, err)
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, "pdf generation failed")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfBytes)
}

// -----------------------------------------------------------------------------
// Search + quick searches (Req 23, 24)
// -----------------------------------------------------------------------------

// handleSearch runs a content search scoped to the caller's accessible Spheres
// (Req 23). The query is the `q` query parameter.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	polys, err := s.search.Search(r.Context(), rc, r.URL.Query().Get("q"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]polygonResponse, 0, len(polys))
	for _, p := range polys {
		out = append(out, toPolygonResponse(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// handleQuickCreated returns "Polygons I've Created" (Req 24.1).
func (s *Server) handleQuickCreated(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	res, err := s.quick.PolygonsICreated(r.Context(), rc)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

// handleQuickRecent returns "Recently Viewed Polygons" (Req 24.2).
func (s *Server) handleQuickRecent(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	res, err := s.quick.RecentlyViewed(r.Context(), rc)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

// -----------------------------------------------------------------------------
// Edit locks (Req 25.3, 25.5)
// -----------------------------------------------------------------------------

// handleAcquireLock acquires the edit lock for a Polygon (Req 25.3). A lock held
// by another user is rejected with LOCKED naming the holder (Req 25.4).
func (s *Server) handleAcquireLock(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	lock, err := s.locks.AcquireLock(r.Context(), rc, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"holderUserId":   lock.HolderUserID,
		"acquiredAt":     lock.AcquiredAt,
		"lastActivityAt": lock.LastActivityAt,
	})
}

// handleReleaseLock releases the caller's edit lock on a Polygon (Req 25.5).
func (s *Server) handleReleaseLock(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.locks.ReleaseLock(r.Context(), rc, id); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleHeartbeatLock refreshes the caller's lock activity so the 300s
// inactivity timeout does not reclaim it while the holder is still editing (Req
// 25.5).
func (s *Server) handleHeartbeatLock(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.locks.RefreshActivity(r.Context(), rc, id); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// -----------------------------------------------------------------------------
// Bookmarks (Req 26)
// -----------------------------------------------------------------------------

// handleListBookmarks returns the caller's bookmarks grouped by owning Sphere
// (Req 26.3, 26.4).
func (s *Server) handleListBookmarks(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	list, err := s.bookmarks.ListBookmarks(r.Context(), rc)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	type group struct {
		Sphere   sphereResponse    `json:"sphere"`
		Polygons []polygonResponse `json:"polygons"`
	}
	groups := make([]group, 0, len(list.Groups))
	for _, g := range list.Groups {
		polys := make([]polygonResponse, 0, len(g.Polygons))
		for _, p := range g.Polygons {
			polys = append(polys, toPolygonResponse(p))
		}
		groups = append(groups, group{Sphere: toSphereResponse(g.Sphere), Polygons: polys})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups, "empty": list.Empty})
}

type addBookmarkRequest struct {
	PolygonRecordID string `json:"polygonRecordId"`
}

// handleAddBookmark bookmarks a Polygon (Req 26.1, 26.2).
func (s *Server) handleAddBookmark(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req addBookmarkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.bookmarks.AddBookmark(r.Context(), rc, req.PolygonRecordID); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRemoveBookmark removes a bookmark (Req 26.5). The {polygonId} param is
// the Polygon Record_ID.
func (s *Server) handleRemoveBookmark(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	polygonID := chi.URLParam(r, "polygonId")
	if err := s.bookmarks.RemoveBookmark(r.Context(), rc, polygonID); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// -----------------------------------------------------------------------------
// Identity (Req 32.1)
// -----------------------------------------------------------------------------

// meResponse is the Identity_Endpoint payload: who the caller is plus their and
// their tenant's Two-Factor_Authentication state (Req 32.1, 33.7).
type meResponse struct {
	UserID            int64  `json:"userId"`
	DisplayName       string `json:"displayName"`
	IsAdmin           bool   `json:"isAdmin"`
	TwoFactorEnrolled bool   `json:"twoFactorEnrolled"`
	TwoFactorRequired bool   `json:"twoFactorRequired"`
}

// handleMe returns the caller's identity for the current session: user id,
// display name, Admin_Group membership, 2FA enrolment status, and the tenant's
// 2FA policy (Req 32.1). It is available to any authenticated session — it sits
// behind the middleware chain but not behind RequireAdmin — so the admin flag is
// reported, not required. The user/tenant ids and admin flag come from the
// request state the chain attached; the display name and 2FA fields are resolved
// from the Central_Directory by the IdentityService.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	rc, authz, ok := authzData(w, r)
	if !ok {
		return
	}
	id, err := s.identity.Me(r.Context(), rc, authz.Admin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, meResponse{
		UserID:            id.UserID,
		DisplayName:       id.DisplayName,
		IsAdmin:           id.IsAdmin,
		TwoFactorEnrolled: id.TwoFactorEnrolled,
		TwoFactorRequired: id.TwoFactorRequired,
	})
}

// -----------------------------------------------------------------------------
// Two-Factor Authentication enrolment (Req 33.1, 33.2, 33.3)
// -----------------------------------------------------------------------------

// twoFactorEnrolResponse is the body of POST /api/2fa/enrol: the base32 secret
// (shown in text form) and the otpauth:// provisioning URI the web application
// renders as a QR (Req 33.1). No QR image bytes are produced server-side.
type twoFactorEnrolResponse struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

// handleTwoFactorEnrol begins Two-Factor Authentication enrolment for the
// authenticated caller (Req 33.1). It sits behind the middleware chain but NOT
// behind RequireAdmin — any authenticated user enrols themselves. The caller is
// identified solely by rc.UserID(), so the endpoint can only ever enrol the
// caller. It generates and stores a pending TOTP_Secret (not yet enforced) and
// returns the secret + provisioning URI.
func (s *Server) handleTwoFactorEnrol(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	secret, uri, err := s.twoFactor.BeginEnrolment(r.Context(), rc.UserID())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, twoFactorEnrolResponse{Secret: secret, URI: uri})
}

// twoFactorConfirmRequest is the body of POST /api/2fa/confirm: the TOTP the
// user read from their authenticator app to prove they scanned the secret.
type twoFactorConfirmRequest struct {
	Code string `json:"code"`
}

// twoFactorConfirmResponse carries the freshly issued single-use Recovery_Codes
// in plaintext, returned exactly ONCE on successful confirmation (Req 33.2). The
// server keeps only Argon2id hashes; this is the caller's sole copy.
type twoFactorConfirmResponse struct {
	RecoveryCodes []string `json:"recoveryCodes"`
}

// handleTwoFactorConfirm confirms enrolment by verifying a submitted TOTP
// against the caller's pending secret (Req 33.2, 33.3). On success it marks the
// caller enrolled and returns the plaintext Recovery_Codes once. An invalid
// code leaves enrolment pending and is mapped to 400 VALIDATION by
// writeServiceError. The caller is identified solely by rc.UserID().
func (s *Server) handleTwoFactorConfirm(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req twoFactorConfirmRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	codes, err := s.twoFactor.ConfirmEnrolment(r.Context(), rc.UserID(), req.Code)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, twoFactorConfirmResponse{RecoveryCodes: codes})
}

// twoFactorPolicyRequest is the body of PUT /api/admin/2fa-policy: whether 2FA
// is required for the tenant (Req 33.7, 33.8).
type twoFactorPolicyRequest struct {
	Required bool `json:"required"`
}

// handleSetTwoFactorPolicy sets the caller's tenant Two_Factor_Policy to
// optional/required (Req 33.7, 33.8). Guarded by RequireAdmin; the tenant is
// taken from the RequestContext, never from client input, so an admin can only
// toggle their own tenant's policy (Req 32.6). When required is set, users who
// are not yet enrolled receive a restricted 'must_enrol' session on their next
// login until they enrol.
func (s *Server) handleSetTwoFactorPolicy(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req twoFactorPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.twoFactor.SetTenantPolicy(r.Context(), rc, req.Required); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"required": req.Required})
}

// -----------------------------------------------------------------------------
// Admin (Req 5, 1.7, 2)
// -----------------------------------------------------------------------------

type createTenantRequest struct {
	Name string `json:"name"`
}

// handleCreateTenant provisions a new tenant transactionally (Req 1.7, 1.8).
// Guarded by RequireAdmin.
func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := s.tenants.CreateTenant(r.Context(), req.Name)
	if err != nil {
		if errors.Is(err, service.ErrTenantNameRequired) {
			writeError(w, http.StatusBadRequest, codeValidation, "tenant name is required")
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, "tenant creation failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tenantId": int64(id)})
}

// handleExportTenant is the admin export endpoint (Req 2). The export writes a
// consistent snapshot to a server-side path; the id param is the tenant id.
func (s *Server) handleExportTenant(w http.ResponseWriter, r *http.Request) {
	rawID := chi.URLParam(r, "id")
	tenantID, err := parseInt64(rawID)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid tenant identifier")
		return
	}
	destPath := r.URL.Query().Get("dest")
	if err := s.tenants.ExportTenant(r.Context(), data.TenantID(tenantID), destPath); err != nil {
		if errors.Is(err, service.ErrExportDestinationRequired) {
			writeError(w, http.StatusBadRequest, codeValidation, "export destination is required")
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, "tenant export failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "dest": destPath})
}

// -----------------------------------------------------------------------------
// Admin user lifecycle (Req 32.2, 5.1, 5.4)
// -----------------------------------------------------------------------------

// adminUserResponse is the JSON projection of a User in the admin listing. It
// mirrors service.AdminUser and deliberately omits the credential/2FA columns.
type adminUserResponse struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Deactivated bool   `json:"deactivated"`
	CreatedAt   string `json:"createdAt"`
}

func toAdminUserResponse(u service.AdminUser) adminUserResponse {
	return adminUserResponse{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Deactivated: u.Deactivated,
		CreatedAt:   u.CreatedAt,
	}
}

// handleListUsers lists the Users in the caller's tenant (Req 32.2). Guarded by
// RequireAdmin; the tenant is taken from the RequestContext, never from client
// input, so the result can only contain the caller's own tenant's users
// (Req 32.6).
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	users, err := s.adminUsers.ListUsers(r.Context(), rc)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]adminUserResponse, 0, len(users))
	for _, u := range users {
		out = append(out, toAdminUserResponse(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

type createUserRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
	GroupID     int64  `json:"groupId"`
}

// handleCreateUser creates a User in the caller's tenant with an initial
// password validated against the shared policy, and adds it to the requested
// Group in the same transaction so the user starts with at least one Group
// membership (Req 32.2, 4.1, 32.7). The Group must belong to the caller's
// tenant; a Group outside it is rejected with TENANT_ISOLATION (Req 32.6). A
// weak password is a VALIDATION error; a create failure leaves no USER row
// behind (Req 32.7).
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u, err := s.adminUsers.CreateUser(r.Context(), rc, req.Username, req.DisplayName, req.Password, data.GroupID(req.GroupID))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAdminUserResponse(u))
}

// handleDeactivateUser deactivates a User in the caller's tenant (Req 5.4). The
// {id} param is the Central USER id. A User outside the caller's tenant is
// rejected with TENANT_ISOLATION (Req 32.6).
func (s *Server) handleDeactivateUser(w http.ResponseWriter, r *http.Request) {
	s.setUserDeactivated(w, r, true)
}

// handleReactivateUser clears the deactivated flag on a User in the caller's
// tenant (Req 5.4). Tenant-scoped like deactivate.
func (s *Server) handleReactivateUser(w http.ResponseWriter, r *http.Request) {
	s.setUserDeactivated(w, r, false)
}

// setUserDeactivated is the shared body of the deactivate/reactivate handlers:
// it reads the RequestContext, parses the {id} param, and delegates to the
// service toggle, which enforces tenant scoping.
func (s *Server) setUserDeactivated(w http.ResponseWriter, r *http.Request, deactivated bool) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	userID, err := parseInt64(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid user identifier")
		return
	}
	if deactivated {
		err = s.adminUsers.Deactivate(r.Context(), rc, userID)
	} else {
		err = s.adminUsers.Reactivate(r.Context(), rc, userID)
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// -----------------------------------------------------------------------------
// Admin Group + membership management (Req 5.2, 32.3)
// -----------------------------------------------------------------------------

// adminGroupResponse is the JSON projection of a Group in the admin listing. It
// mirrors service.AdminGroup.
type adminGroupResponse struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"isAdmin"`
}

func toAdminGroupResponse(g service.AdminGroup) adminGroupResponse {
	return adminGroupResponse{ID: g.ID, Name: g.Name, IsAdmin: g.IsAdmin}
}

// handleListGroups lists the Groups in the caller's tenant (Req 5.2). Guarded by
// RequireAdmin; the tenant is taken from the RequestContext, never from client
// input, so the result can only contain the caller's own tenant's Groups
// (Req 32.6).
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	groups, err := s.userGroups.ListGroups(r.Context(), rc)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]adminGroupResponse, 0, len(groups))
	for _, g := range groups {
		out = append(out, toAdminGroupResponse(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

type createGroupRequest struct {
	Name string `json:"name"`
}

// handleCreateGroup creates a Group in the caller's tenant (Req 5.2). The tenant
// is taken from the RequestContext (Req 32.6); an empty or duplicate name is a
// VALIDATION error.
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req createGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	g, err := s.userGroups.CreateGroup(r.Context(), rc, req.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAdminGroupResponse(g))
}

type groupMemberRequest struct {
	UserID int64 `json:"userId"`
}

// handleAddGroupMember adds a User to the Group named by the {id} path param
// (Req 5.2, 32.3). Both the Group and the User must belong to the caller's
// tenant; a cross-tenant reference is rejected with TENANT_ISOLATION (Req 32.6).
func (s *Server) handleAddGroupMember(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	groupID, err := parseInt64(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid group identifier")
		return
	}
	var req groupMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.userGroups.AddMembershipScoped(r.Context(), rc, data.UserID(req.UserID), data.GroupID(groupID)); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRemoveGroupMember removes a User from the Group named by the {id} path
// param (Req 5.2, 32.3). Both the Group and the User must belong to the caller's
// tenant; a cross-tenant reference is rejected with TENANT_ISOLATION (Req 32.6).
// A removal that would leave the User with zero Groups is a VALIDATION error
// (Req 4.1).
func (s *Server) handleRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	groupID, err := parseInt64(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid group identifier")
		return
	}
	userID, err := parseInt64(chi.URLParam(r, "userId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid user identifier")
		return
	}
	if err := s.userGroups.RemoveMembershipScoped(r.Context(), rc, data.UserID(userID), data.GroupID(groupID)); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// -----------------------------------------------------------------------------
// Admin per-Sphere Group access matrix (Req 5.3, 32.4)
// -----------------------------------------------------------------------------

// sphereGrantResponse is the JSON projection of a single SPHERE_GRANT row in the
// admin sphere-access listing. It mirrors service.SphereGrantEntry, speaking the
// external Sphere Record_ID and rendering the access level as its text form.
type sphereGrantResponse struct {
	SphereRecordID string `json:"sphereRecordId"`
	GroupID        int64  `json:"groupId"`
	Access         string `json:"access"`
	Reveal         bool   `json:"reveal"`
}

func toSphereGrantResponse(g service.SphereGrantEntry) sphereGrantResponse {
	return sphereGrantResponse{
		SphereRecordID: g.SphereRecordID,
		GroupID:        int64(g.Group),
		Access:         accessLevelToText(g.Access),
		Reveal:         g.Reveal,
	}
}

// handleListSphereAccess lists the per-Sphere Group grants in the caller's
// tenant (Req 5.3, 32.4). Guarded by RequireAdmin; the tenant is taken from the
// RequestContext, never from client input, so the result can only contain the
// caller's own tenant's grants (Req 32.6).
func (s *Server) handleListSphereAccess(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	grants, err := s.userGroups.ListSphereGrants(r.Context(), rc)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]sphereGrantResponse, 0, len(grants))
	for _, g := range grants {
		out = append(out, toSphereGrantResponse(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

// grantSphereAccessRequest is the body of PUT /api/admin/sphere-access: the
// Sphere Record_ID (canonical or Short form), the Central Group id, the access
// level ("read" | "write"), and the Reveal flag.
type grantSphereAccessRequest struct {
	SphereRecordID string `json:"sphereRecordId"`
	GroupID        int64  `json:"groupId"`
	Access         string `json:"access"`
	Reveal         bool   `json:"reveal"`
}

// handleGrantSphereAccess grants a Group access to a Sphere at the requested
// level with the requested Reveal flag, in the caller's tenant (Req 5.3, 32.4).
// Both the Sphere and the Group must belong to the caller's tenant; a
// cross-tenant reference is rejected with TENANT_ISOLATION (Req 32.6). An access
// level other than read or write is a VALIDATION error (Req 4.2). Re-granting an
// existing (Sphere, Group) pair updates the level and Reveal.
func (s *Server) handleGrantSphereAccess(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req grantSphereAccessRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sphere, err := parseRecordID(req.SphereRecordID)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid sphere identifier")
		return
	}
	access, ok := accessLevelFromText(req.Access)
	if !ok {
		writeError(w, http.StatusBadRequest, codeValidation, "grant access level must be read or write")
		return
	}
	if err := s.userGroups.GrantSphereByRecordID(r.Context(), rc, sphere, data.GroupID(req.GroupID), access, req.Reveal); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// revokeSphereAccessRequest is the body of DELETE /api/admin/sphere-access: the
// Sphere Record_ID and the Central Group id whose grant is being removed.
type revokeSphereAccessRequest struct {
	SphereRecordID string `json:"sphereRecordId"`
	GroupID        int64  `json:"groupId"`
}

// handleRevokeSphereAccess removes a Group's grant on a Sphere in the caller's
// tenant (Req 5.3, 32.4, 4.6). Both the Sphere and the Group must belong to the
// caller's tenant; a cross-tenant reference is rejected with TENANT_ISOLATION
// (Req 32.6). Revoking a grant that does not exist is a no-op success.
func (s *Server) handleRevokeSphereAccess(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	var req revokeSphereAccessRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sphere, err := parseRecordID(req.SphereRecordID)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid sphere identifier")
		return
	}
	if err := s.userGroups.RevokeSphereByRecordID(r.Context(), rc, sphere, data.GroupID(req.GroupID)); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// accessLevelToText renders an AccessLevel as the text form the API speaks. Only
// read and write are valid grant levels; anything else (AccessNone or unknown)
// renders as "none" so a malformed stored row is visible rather than silently
// omitted.
func accessLevelToText(a data.AccessLevel) string {
	switch a {
	case data.AccessWrite:
		return "write"
	case data.AccessRead:
		return "read"
	default:
		return "none"
	}
}

// accessLevelFromText parses the API access-level text into an AccessLevel,
// accepting only "read" and "write" (Req 4.2). ok=false signals an invalid level
// the handler surfaces as VALIDATION before touching the service.
func accessLevelFromText(s string) (data.AccessLevel, bool) {
	switch s {
	case "read":
		return data.AccessRead, true
	case "write":
		return data.AccessWrite, true
	default:
		return data.AccessNone, false
	}
}

// -----------------------------------------------------------------------------
// WebSocket edit endpoint (Req 25.2)
// -----------------------------------------------------------------------------

// handleWebSocket upgrades the connection and joins the per-Polygon room for
// collaborative edits / lock signaling (Req 25.2). The Polygon's mode is
// resolved within the caller's tenant first, so a Polygon in another tenant is
// rejected before any room is joined (Req 1.6).
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	key := ws.RoomKey{TenantID: rc.TenantID(), PolygonRecordID: id}
	mode, err := ws.ResolveMode(r.Context(), s.modeResolver, rc, key)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept already wrote an error response on failure.
		return
	}
	// Serve blocks until the client disconnects; use the request context so a
	// server shutdown / client cancel tears the room down.
	s.hub.Serve(r.Context(), key, mode, conn)
}

// -----------------------------------------------------------------------------
// Shared param helpers
// -----------------------------------------------------------------------------

// parseRecordID parses a Record_ID from its canonical or Short-UUID form. URLs
// carry the Short form (Req 15.2) but the canonical form is also accepted so the
// same helper serves API bodies that echo canonical ids.
func parseRecordID(raw string) (recordid.ID, error) {
	if id, err := recordid.Parse(raw); err == nil {
		return id, nil
	}
	return recordid.ParseShort(raw)
}

// parseRecordIDParam reads a chi URL param and parses it as a Record_ID, writing
// a VALIDATION envelope and returning ok=false on failure.
func parseRecordIDParam(w http.ResponseWriter, r *http.Request, name string) (recordid.ID, bool) {
	id, err := parseRecordID(chi.URLParam(r, name))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid identifier")
		return recordid.ID{}, false
	}
	return id, true
}

// parseInt64 parses a base-10 signed 64-bit integer for numeric URL params
// (e.g. the admin tenant id). It rejects any non-integer value.
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
