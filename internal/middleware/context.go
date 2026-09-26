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

// This file defines the request-scoped values the middleware chain attaches to
// the standard library request context.Context and the typed accessors handlers
// use to read them back. The chain resolves the caller's identity, tenant, and
// authorization once (design.md — "Request / Authorization Middleware") and
// stores the results here so downstream handlers and services trust the
// RequestContext and never re-derive the tenant from client input.

import (
	"context"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/service"
)

// ctxKey is an unexported type for the context keys defined here. Using a
// private type prevents collisions with keys set by other packages, since no
// other package can construct a value of this type.
type ctxKey int

const (
	// requestContextKey carries the immutable data.RequestContext built by the
	// chain after session validation, tenant resolution, and authorization.
	requestContextKey ctxKey = iota
	// authzDataKey carries the raw service.AuthzData the Authz stage loaded for
	// the request. Handlers that need to re-check a per-Sphere decision (for a
	// Sphere id only known once the body is parsed) can evaluate the
	// PolicyEngine against it without another database round-trip.
	authzDataKey
)

// withRequestContext returns a child context carrying rc.
func withRequestContext(ctx context.Context, rc data.RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey, rc)
}

// withAuthzData returns a child context carrying authz.
func withAuthzData(ctx context.Context, authz service.AuthzData) context.Context {
	return context.WithValue(ctx, authzDataKey, authz)
}

// RequestContextFrom returns the RequestContext attached by the middleware
// chain and whether one is present. It is absent only when a handler runs
// outside the authenticated chain (for example a public route), so callers that
// require authentication should treat !ok as an internal error.
func RequestContextFrom(ctx context.Context) (data.RequestContext, bool) {
	rc, ok := ctx.Value(requestContextKey).(data.RequestContext)
	return rc, ok
}

// AuthzDataFrom returns the AuthzData the Authz stage loaded for the request and
// whether it is present. Handlers use it to evaluate per-Sphere PolicyEngine
// decisions for a Sphere id that is only known after request parsing.
func AuthzDataFrom(ctx context.Context) (service.AuthzData, bool) {
	authz, ok := ctx.Value(authzDataKey).(service.AuthzData)
	return authz, ok
}
