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

// The middleware chain speaks the same standard error envelope the API layer
// uses (design.md — "Standard error envelope: { error: { code, message } }").
// Emitting the shared shape here keeps a 401/403 raised by the chain
// indistinguishable in structure from a domain error raised by a handler, so
// clients parse one response format. The full envelope with all domain codes is
// owned by task 22.2; this file provides just the writer and the codes the
// chain itself raises (ADMIN_REQUIRED for Req 5.5, plus the auth/sphere-access
// denials).

import (
	"encoding/json"
	"net/http"
	"strconv"
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

// writeError writes the standard error envelope with the given HTTP status,
// machine code, and message. It sets the JSON content type and status before
// encoding so the response is well-formed even if encoding somehow fails
// mid-write.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{
		Error: errorPayload{Code: code, Message: message},
	})
}

// parseInt64 parses a base-10 signed 64-bit integer. It is the small helper the
// Sphere-access guard uses to turn a URL parameter into a tenant-local SphereID.
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
