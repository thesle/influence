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

// The Authz stage of the middleware chain needs the caller's raw authorization
// inputs before the PolicyEngine can decide anything: which Groups the caller
// belongs to, whether any is the Admin_Group, whether the account is
// deactivated, and — for those Groups — the per-Sphere SPHERE_GRANT rows in the
// caller's tenant (design.md — "Authorization Middleware — Req 4, 5"). This file
// owns loading that data from the two tiers of the data model:
//
//   - Group memberships, the Admin_Group flag, and the deactivated flag come
//     from the Central_Directory (GROUP / GROUP_MEMBERSHIP / USER) — identity
//     lives in Central, never in a tenant file.
//   - The per-Group SPHERE_GRANT rows come from the caller's Tenant_Database,
//     reached only through ConnManager.Tenant(ctx) so the load is structurally
//     confined to the caller's own tenant (Requirements 1.4, 1.5).
//
// The loader returns the caller's Group id set alongside the AuthzData so the
// Authz stage can build the RequestContext (which carries Groups) without a
// second query.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/service"
)

// AuthzLoader loads the raw authorization inputs for a request from the two
// data tiers. It is an interface so the chain can be exercised in tests with a
// fake loader and so the concrete, database-backed loader stays swappable.
type AuthzLoader interface {
	// Load returns the caller's Group memberships and the AuthzData the
	// PolicyEngine reduces to decisions. userID identifies the caller in the
	// Central_Directory; the tenant handle for the caller's own tenant is
	// resolved from rc via the connection manager.
	Load(ctx context.Context, rc data.RequestContext) ([]data.GroupID, service.AuthzData, error)
}

// dbAuthzLoader is the database-backed AuthzLoader. It reads identity state from
// the shared Central_Directory handle and the per-Group grants from the caller's
// tenant database, which it obtains from the connection manager.
type dbAuthzLoader struct {
	conns data.ConnManager
}

// NewAuthzLoader builds the default database-backed AuthzLoader over the
// connection manager. Central identity comes from conns.Central(); tenant
// grants come from conns.Tenant(ctx, rc), which resolves ONLY the caller's own
// tenant.
func NewAuthzLoader(conns data.ConnManager) AuthzLoader {
	return dbAuthzLoader{conns: conns}
}

// Load assembles the caller's AuthzData:
//
//  1. Read the deactivated flag from Central USER.
//  2. Read the caller's Group memberships joined to GROUP, capturing each
//     group id and whether it is the Admin_Group (is_admin = 1). Admin status is
//     the OR of is_admin across the caller's Groups.
//  3. Read every SPHERE_GRANT row in the caller's tenant whose group_id is one
//     of the caller's Groups, mapping each to a service.GroupGrant.
//
// The Sphere grants are loaded even for an admin (whose implicit grant makes
// them redundant) and even for a deactivated user (whose access the engine
// denies regardless) so the returned AuthzData is a faithful, engine-agnostic
// snapshot; the PolicyEngine applies the admin/deactivated precedence rules.
func (l dbAuthzLoader) Load(ctx context.Context, rc data.RequestContext) ([]data.GroupID, service.AuthzData, error) {
	central := l.conns.Central()

	deactivated, err := loadDeactivated(ctx, central, rc.UserID())
	if err != nil {
		return nil, service.AuthzData{}, err
	}

	groups, admin, err := loadGroups(ctx, central, rc.UserID())
	if err != nil {
		return nil, service.AuthzData{}, err
	}

	grants, err := l.loadGrants(ctx, rc, groups)
	if err != nil {
		return nil, service.AuthzData{}, err
	}

	return groups, service.AuthzData{
		Admin:       admin,
		Deactivated: deactivated,
		Grants:      grants,
	}, nil
}

// loadDeactivated reads the deactivated flag for a user from Central. An unknown
// user is treated as deactivated (fail closed): a session that resolved to a
// user row that has since disappeared should reach nothing.
func loadDeactivated(ctx context.Context, central *sql.DB, userID data.UserID) (bool, error) {
	var deactivated int
	err := central.QueryRowContext(ctx,
		`SELECT deactivated FROM "USER" WHERE id = ?`, int64(userID),
	).Scan(&deactivated)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("load user deactivated flag: %w", err)
	}
	return deactivated != 0, nil
}

// loadGroups reads the caller's Group memberships from Central, returning the
// group id set and whether any is the Admin_Group. A user with no memberships
// yields an empty set and admin=false; the minimum-one-membership invariant
// (Req 4.1) is enforced elsewhere, so this stage simply reflects the stored
// state.
func loadGroups(ctx context.Context, central *sql.DB, userID data.UserID) ([]data.GroupID, bool, error) {
	rows, err := central.QueryContext(ctx,
		`SELECT g.id, g.is_admin
		   FROM "GROUP_MEMBERSHIP" m
		   JOIN "GROUP" g ON g.id = m.group_id
		  WHERE m.user_id = ?`,
		int64(userID),
	)
	if err != nil {
		return nil, false, fmt.Errorf("load group memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		groups []data.GroupID
		admin  bool
	)
	for rows.Next() {
		var (
			id      int64
			isAdmin int
		)
		if err := rows.Scan(&id, &isAdmin); err != nil {
			return nil, false, fmt.Errorf("scan group membership: %w", err)
		}
		groups = append(groups, data.GroupID(id))
		if isAdmin != 0 {
			admin = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate group memberships: %w", err)
	}
	return groups, admin, nil
}

// loadGrants reads the per-Group SPHERE_GRANT rows for the caller's Groups from
// the caller's tenant database. The tenant handle is resolved only for
// rc.TenantID via the connection manager (Req 1.4, 1.5). A caller in no Group
// has no grants, so the tenant is not even opened.
func (l dbAuthzLoader) loadGrants(ctx context.Context, rc data.RequestContext, groups []data.GroupID) ([]service.GroupGrant, error) {
	if len(groups) == 0 {
		return nil, nil
	}

	tenantDB, err := l.conns.Tenant(ctx, rc)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant database: %w", err)
	}

	rows, err := tenantDB.DB().QueryContext(ctx,
		`SELECT sphere_id, group_id, access, reveal FROM sphere_grant`,
	)
	if err != nil {
		return nil, fmt.Errorf("load sphere grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Build a membership set so the query result can be filtered to the
	// caller's Groups in memory. Filtering here (rather than with a dynamic IN
	// clause) keeps the query static and injection-free while still confining
	// the effective grants to the caller's own Groups.
	member := make(map[data.GroupID]struct{}, len(groups))
	for _, g := range groups {
		member[g] = struct{}{}
	}

	var grants []service.GroupGrant
	for rows.Next() {
		var (
			sphereID int64
			groupID  int64
			access   string
			reveal   int
		)
		if err := rows.Scan(&sphereID, &groupID, &access, &reveal); err != nil {
			return nil, fmt.Errorf("scan sphere grant: %w", err)
		}
		if _, ok := member[data.GroupID(groupID)]; !ok {
			continue
		}
		grants = append(grants, service.GroupGrant{
			Group:  data.GroupID(groupID),
			Sphere: data.SphereID(sphereID),
			Access: parseAccessLevel(access),
			Reveal: reveal != 0,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sphere grants: %w", err)
	}
	return grants, nil
}

// parseAccessLevel maps the stored SPHERE_GRANT.access text to an AccessLevel.
// An unrecognized value maps to AccessNone (fail closed): a grant row the schema
// did not anticipate confers no access rather than accidental write.
func parseAccessLevel(s string) data.AccessLevel {
	switch s {
	case "write":
		return data.AccessWrite
	case "read":
		return data.AccessRead
	default:
		return data.AccessNone
	}
}
