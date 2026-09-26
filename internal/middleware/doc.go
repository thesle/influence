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

// Package middleware holds the ordered request chain — TLS-enforce, Session,
// Tenant-resolve, and Authz — plus the route-level authorization guards
// (RequireAdmin, RequireSphereAccess) that hang off it. The chain validates the
// session, resolves the caller's tenant from the session (never from client
// input), loads the caller's Group memberships and per-Sphere grants, and
// attaches an immutable data.RequestContext that flows to every downstream
// service call (design.md — "Request / Authorization Middleware"; Requirements
// 1.4, 3.5, 3.6, 4.5, 5.5).
package middleware
