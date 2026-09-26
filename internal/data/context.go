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

package data

// This file defines the immutable request-scoped identity and authorization
// context that flows from the middleware chain into every service and
// repository call (design.md — "Request / Authorization Middleware"). It is the
// sole carrier of the caller's authenticated tenant: the connection manager
// resolves a tenant handle from ctx.TenantID and nothing else, which is what
// makes tenant isolation structural rather than a convention (Requirements 1.4,
// 1.5).

// TenantID identifies a single tenant. It matches the integer surrogate primary
// key of the Central_Directory TENANT table. It is a distinct type so a tenant
// id can never be silently confused with a user id, group id, or any other
// integer key at a call site.
type TenantID int64

// UserID identifies a single authenticated user (the Central_Directory USER
// primary key). Distinct from TenantID for the same reason.
type UserID int64

// GroupID identifies a Group in the Central_Directory (GROUP primary key). A
// user's effective per-Sphere access is the union of the grants of the Groups
// it belongs to (Requirement 4.3), so the context carries the caller's Group
// set.
type GroupID int64

// SphereID identifies a Sphere within the caller's tenant. Sphere grants are
// keyed by SphereID.
type SphereID int64

// AccessLevel is the access a grant confers on a Sphere. The zero value,
// AccessNone, denies access; the effective access for a Sphere is the
// most-permissive across the caller's Groups (Requirement 4.3).
type AccessLevel int

const (
	// AccessNone means the caller has no access to the Sphere. It is the zero
	// value so an absent grant defaults to "no access".
	AccessNone AccessLevel = iota
	// AccessRead permits reading the Sphere's content.
	AccessRead
	// AccessWrite permits reading and writing the Sphere's content. Write
	// implies read.
	AccessWrite
)

// SphereGrant is the caller's effective authorization for a single Sphere: the
// access level and whether Sensitive_Data may be revealed within that Sphere.
// Reveal is per-Sphere (Requirement 16.6), so it travels alongside the access
// level rather than as a tenant-wide flag.
type SphereGrant struct {
	Access AccessLevel
	Reveal bool
}

// RequestContext is the immutable identity + authorization context attached by
// the middleware chain after session validation and authorization. Downstream
// services and repositories trust it and never re-derive the tenant from
// client input (design.md — "Downstream services trust RequestContext").
//
// It is immutable by construction: fields are unexported, the struct is
// constructed once via NewRequestContext, and the accessors return copies of
// the slice/map contents so a holder cannot mutate the authorization state a
// caller was authenticated with.
type RequestContext struct {
	userID       UserID
	tenantID     TenantID
	groups       []GroupID
	sphereGrants map[SphereID]SphereGrant
}

// NewRequestContext builds an immutable RequestContext. The supplied groups and
// sphereGrants are copied so later mutation of the caller's inputs cannot alter
// the context after construction. A nil groups or sphereGrants is treated as
// empty.
func NewRequestContext(userID UserID, tenantID TenantID, groups []GroupID, sphereGrants map[SphereID]SphereGrant) RequestContext {
	groupsCopy := make([]GroupID, len(groups))
	copy(groupsCopy, groups)

	grantsCopy := make(map[SphereID]SphereGrant, len(sphereGrants))
	for k, v := range sphereGrants {
		grantsCopy[k] = v
	}

	return RequestContext{
		userID:       userID,
		tenantID:     tenantID,
		groups:       groupsCopy,
		sphereGrants: grantsCopy,
	}
}

// UserID returns the authenticated user's id.
func (c RequestContext) UserID() UserID { return c.userID }

// TenantID returns the authenticated tenant's id. This is the ONLY tenant a
// caller holding this context may reach through the connection manager.
func (c RequestContext) TenantID() TenantID { return c.tenantID }

// Groups returns a copy of the caller's Group memberships. A copy is returned so
// the caller cannot mutate the context's internal state.
func (c RequestContext) Groups() []GroupID {
	out := make([]GroupID, len(c.groups))
	copy(out, c.groups)
	return out
}

// SphereGrant returns the caller's effective grant for a Sphere and whether a
// grant exists. An absent grant means no access (AccessNone).
func (c RequestContext) SphereGrant(id SphereID) (SphereGrant, bool) {
	g, ok := c.sphereGrants[id]
	return g, ok
}

// SphereGrants returns a copy of the caller's full per-Sphere grant map so the
// internal map cannot be mutated by a holder.
func (c RequestContext) SphereGrants() map[SphereID]SphereGrant {
	out := make(map[SphereID]SphereGrant, len(c.sphereGrants))
	for k, v := range c.sphereGrants {
		out[k] = v
	}
	return out
}
