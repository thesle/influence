// Package middleware holds the ordered request chain — TLS-enforce, Session,
// Tenant-resolve, and Authz — plus the route-level authorization guards
// (RequireAdmin, RequireSphereAccess) that hang off it. The chain validates the
// session, resolves the caller's tenant from the session (never from client
// input), loads the caller's Group memberships and per-Sphere grants, and
// attaches an immutable data.RequestContext that flows to every downstream
// service call (design.md — "Request / Authorization Middleware"; Requirements
// 1.4, 3.5, 3.6, 4.5, 5.5).
package middleware
