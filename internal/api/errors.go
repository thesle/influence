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

// The REST layer speaks one standard error envelope for every failure, matching
// the shape the middleware chain already emits (design.md — "Standard error
// envelope: { error: { code, message } }" and the "Error Handling" section).
// Emitting one shape from the chain and the handlers means a client parses a
// single response format whether a request was rejected at authentication, at a
// route guard, or inside a handler.
//
// This file owns the envelope writer and the mapping from the domain error
// sentinels the service/repo layers return to their (HTTP status, code) pair.
// Handlers stay thin: they call the service, and on error hand the error to
// writeServiceError, which picks the right envelope. Task 22.2 may formalize
// the envelope further; this is the shared helper the wiring task needs.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/influence/influence/internal/repo"
	"github.com/influence/influence/internal/service"
)

// errorBody is the standard error envelope: a single "error" object carrying a
// machine-readable code and a human-readable message.
type errorBody struct {
	Error errorPayload `json:"error"`
}

// errorPayload is the inner { code, message } object.
type errorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes emitted by the REST layer. They mirror the design's "Error
// Handling" categories so a client can branch on a stable machine code rather
// than an HTTP status alone.
const (
	codeValidation      = "VALIDATION"
	codeTenantIsolation = "TENANT_ISOLATION"
	codeHierarchy       = "HIERARCHY"
	codeLocked          = "LOCKED"
	codeRevealDenied    = "REVEAL_DENIED"
	codeRevealFailed    = "REVEAL_FAILED"
	codeAdminRequired   = "ADMIN_REQUIRED"
	codeSetupComplete   = "SETUP_COMPLETE"
	codeEnrolmentReq    = "ENROLMENT_REQUIRED"
	codeUnauthenticated = "UNAUTHENTICATED"
	codeNotFound        = "NOT_FOUND"
	codeBadRequest      = "BAD_REQUEST"
	codeInternal        = "INTERNAL"
)

// writeError writes the standard error envelope with the given HTTP status,
// machine code, and message. It sets the JSON content type and status before
// encoding so the response is well-formed even if encoding fails mid-write.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorPayload{Code: code, Message: message}})
}

// writeServiceError maps a domain error returned by the service/repo layer to
// the appropriate envelope. The mapping follows the design's "Error Handling"
// table:
//
//   - ErrNotAccessible → 404 TENANT_ISOLATION (a record outside the caller's
//     tenant is indistinguishable from a nonexistent one — Req 1.6).
//   - ErrValidation → 400 VALIDATION (name/type/url rules — Req 6.4, 10.3, …).
//   - ErrUserNotAccessible / ErrGroupNotAccessible → 404 TENANT_ISOLATION (an
//     admin reference to a user/group outside the caller's tenant — Req 32.6).
//   - ErrPasswordPolicy → 400 VALIDATION (password policy — Req 31.5, 32.2).
//   - ErrUsernameRequired / ErrUsernameTaken → 400 VALIDATION (admin user
//     create — Req 32.2).
//   - ErrGroupNameRequired / ErrGroupNameTaken → 400 VALIDATION (admin group
//     create — Req 5.2).
//   - ErrSphereNotAccessible → 404 TENANT_ISOLATION (a grant/revoke referencing
//     a Sphere outside the caller's tenant — Req 5.3, 32.6).
//   - ErrInvalidAccessLevel → 400 VALIDATION (a grant level that is neither read
//     nor write — Req 4.2, 5.3).
//   - ErrMinGroupMembership → 400 VALIDATION (a removal that would leave a User
//     with zero Groups — Req 4.1).
//   - ErrHierarchy → 409 HIERARCHY (self/descendant/cross-sphere reparent —
//     Req 9).
//   - ErrLocked → 409 LOCKED (edit of a locked Polygon — Req 25.4).
//   - ErrRevealDenied → 403 REVEAL_DENIED (no reveal permission — Req 16.5).
//   - ErrRevealFailed → 409 REVEAL_FAILED (missing/undecryptable ciphertext —
//     Req 16.8).
//   - image format/size sentinels → 400 VALIDATION (Req 17.2, 17.3).
//
// Anything else is an unexpected internal failure and maps to 500 INTERNAL with
// a generic message so no internal detail leaks to the client.
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repo.ErrNotAccessible):
		writeError(w, http.StatusNotFound, codeTenantIsolation, "not accessible")
	case errors.Is(err, service.ErrPolygonNotAccessible):
		writeError(w, http.StatusNotFound, codeTenantIsolation, "not accessible")
	case errors.Is(err, service.ErrUserNotAccessible):
		writeError(w, http.StatusNotFound, codeTenantIsolation, "not accessible")
	case errors.Is(err, service.ErrGroupNotAccessible):
		writeError(w, http.StatusNotFound, codeTenantIsolation, "not accessible")
	case errors.Is(err, service.ErrSphereNotAccessible):
		writeError(w, http.StatusNotFound, codeTenantIsolation, "not accessible")
	case errors.Is(err, service.ErrSetupComplete):
		// The tenant already has an administrator, so the one-time first-run
		// setup is closed. The request was well-formed, so this is neither a
		// validation nor an auth failure: it is a 409 SETUP_COMPLETE (Req 31.4,
		// 31.6).
		writeError(w, http.StatusConflict, codeSetupComplete, "setup has already been completed")
	case errors.Is(err, service.ErrUsernameRequired):
		writeError(w, http.StatusBadRequest, codeValidation, "username is required")
	case errors.Is(err, service.ErrUsernameTooLong):
		writeError(w, http.StatusBadRequest, codeValidation, "username must be at most 100 characters")
	case errors.Is(err, service.ErrUsernameTaken):
		writeError(w, http.StatusBadRequest, codeValidation, err.Error())
	case errors.Is(err, service.ErrGroupNameRequired):
		writeError(w, http.StatusBadRequest, codeValidation, "group name is required")
	case errors.Is(err, service.ErrGroupNameTaken):
		writeError(w, http.StatusBadRequest, codeValidation, err.Error())
	case errors.Is(err, service.ErrInvalidAccessLevel):
		// A grant must confer a concrete level (read or write); AccessNone is
		// the absence of a grant, not a grant, so an invalid/none level is a
		// VALIDATION-class rejection (Req 4.2, 5.3).
		writeError(w, http.StatusBadRequest, codeValidation, "grant access level must be read or write")
	case errors.Is(err, service.ErrMinGroupMembership):
		// A removal that would leave a User with zero Groups violates the
		// min-one-Group invariant (Req 4.1). It is a VALIDATION-class rejection;
		// the message names the specific rule so a client can distinguish it.
		writeError(w, http.StatusBadRequest, codeValidation, "MIN_GROUP_MEMBERSHIP: a user must belong to at least one group")
	case errors.Is(err, repo.ErrValidation):
		writeError(w, http.StatusBadRequest, codeValidation, err.Error())
	case errors.Is(err, service.ErrInvalidSpan):
		writeError(w, http.StatusBadRequest, codeValidation, "invalid span range")
	case errors.Is(err, service.ErrPasswordPolicy):
		writeError(w, http.StatusBadRequest, codeValidation, err.Error())
	case errors.Is(err, service.ErrTwoFactorInvalidCode):
		// A submitted TOTP that does not match the pending secret leaves
		// enrolment pending and is a VALIDATION-class rejection (Req 33.3).
		writeError(w, http.StatusBadRequest, codeValidation, "invalid two-factor code")
	case errors.Is(err, service.ErrTwoFactorNoPendingSecret):
		// Confirm was called before begin: there is no pending TOTP_Secret to
		// verify against, so the request is out of sequence (Req 33.1, 33.2).
		writeError(w, http.StatusBadRequest, codeValidation, "no pending two-factor enrolment")
	case errors.Is(err, repo.ErrHierarchy):
		writeError(w, http.StatusConflict, codeHierarchy, err.Error())
	case errors.Is(err, service.ErrRevealDenied):
		writeError(w, http.StatusForbidden, codeRevealDenied, "reveal not permitted for this sphere")
	case errors.Is(err, service.ErrRevealFailed):
		writeError(w, http.StatusConflict, codeRevealFailed, "reveal failed")
	case errors.Is(err, repo.ErrUnsupportedImageFormat):
		writeError(w, http.StatusBadRequest, codeValidation, "unsupported image format")
	case errors.Is(err, repo.ErrImageTooLarge):
		writeError(w, http.StatusBadRequest, codeValidation, "image exceeds maximum allowed size")
	default:
		// A record-locking rejection carries the holder identity, so it is
		// matched with errors.As and rendered with its own message (Req 25.4).
		var locked repo.ErrLocked
		if errors.As(err, &locked) {
			writeError(w, http.StatusConflict, codeLocked, locked.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
	}
}
