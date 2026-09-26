package service

// The admin Group half of Requirement 5.2 / 32.3: an Admin_Group member lists
// the Groups in their Tenant, creates a Group, and manages who belongs to a
// Group. These are the operations the admin Group routes (GET/POST
// /api/admin/groups and .../{id}/members) delegate to, keeping those handlers
// thin.
//
// Where the state lives. Identity is platform-wide, so GROUP and
// GROUP_MEMBERSHIP live in the Central_Directory (Requirement 1.1); every query
// here runs against conns.Central(). A Group's tenant is the tenant_id column on
// its GROUP row, not a separate database.
//
// Tenant scoping is structural, never from client input (Requirement 32.6). The
// caller's tenant is read from the RequestContext; every read is filtered by
// that tenant_id and every membership mutation first confirms both the target
// Group and the target User belong to it. A Group or User outside the caller's
// tenant is indistinguishable from a nonexistent one and is rejected with
// ErrGroupNotAccessible / ErrUserNotAccessible, which the REST layer maps to the
// TENANT_ISOLATION envelope (Req 1.6, 32.6).
//
// The min-one-Group invariant (Requirement 4.1) is enforced by the underlying
// RemoveMembership: a removal that would drop a User to zero Groups returns
// ErrMinGroupMembership, which the REST layer maps to a VALIDATION envelope.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/influence/influence/internal/data"
)

// ErrGroupNameRequired is returned when a Group create is missing a name. A
// GROUP row requires a non-empty name unique within its tenant, so an empty
// value is a VALIDATION-class rejection.
var ErrGroupNameRequired = errors.New("group name is required")

// ErrGroupNameTaken is returned when a Group create would duplicate a name
// within the caller's tenant (the GROUP (tenant_id, name) uniqueness). It is a
// VALIDATION-class error so the REST layer can surface it to the admin.
var ErrGroupNameTaken = errors.New("group name already exists in this tenant")

// AdminGroup is the projection of a Central_Directory GROUP row the admin Group
// listing returns. It carries the identity and Admin_Group flag an
// administration page needs.
type AdminGroup struct {
	// ID is the Central_Directory GROUP primary key.
	ID int64
	// Name is unique within the tenant.
	Name string
	// IsAdmin reports whether this is the reserved Admin_Group (Req 4.7, 5).
	IsAdmin bool
}

// ListGroups returns every Group in the caller's tenant, ordered by name for a
// stable listing (Req 5.2). The tenant is taken from the RequestContext, never
// from client input, so the result can only ever contain the caller's own
// tenant's Groups (Req 32.6).
func (s *UserGroupService) ListGroups(ctx context.Context, rc data.RequestContext) ([]AdminGroup, error) {
	rows, err := s.conns.Central().QueryContext(ctx,
		`SELECT id, name, is_admin FROM "GROUP" WHERE tenant_id = ? ORDER BY name`,
		int64(rc.TenantID()),
	)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AdminGroup
	for rows.Next() {
		var (
			g       AdminGroup
			isAdmin int
		)
		if err := rows.Scan(&g.ID, &g.Name, &isAdmin); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		g.IsAdmin = isAdmin != 0
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate groups: %w", err)
	}
	return out, nil
}

// CreateGroup creates a non-admin Group in the caller's tenant with the given
// name (Req 5.2). The tenant is taken from the RequestContext (Req 32.6). An
// empty name is ErrGroupNameRequired and a duplicate name within the tenant is
// ErrGroupNameTaken, both VALIDATION-class; the duplicate is checked in the same
// transaction as the insert so the create is atomic and the raw UNIQUE
// violation never surfaces as an internal error.
func (s *UserGroupService) CreateGroup(ctx context.Context, rc data.RequestContext, name string) (AdminGroup, error) {
	if name == "" {
		return AdminGroup{}, ErrGroupNameRequired
	}
	tenantID := int64(rc.TenantID())

	tx, err := s.conns.Central().BeginTx(ctx, nil)
	if err != nil {
		return AdminGroup{}, fmt.Errorf("begin create group tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var taken int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "GROUP" WHERE tenant_id = ? AND name = ?`,
		tenantID, name,
	).Scan(&taken); err != nil {
		return AdminGroup{}, fmt.Errorf("check group name: %w", err)
	}
	if taken > 0 {
		return AdminGroup{}, ErrGroupNameTaken
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO "GROUP"(tenant_id, name, is_admin) VALUES(?, ?, 0)`,
		tenantID, name,
	)
	if err != nil {
		return AdminGroup{}, fmt.Errorf("insert group: %w", err)
	}
	groupID, err := res.LastInsertId()
	if err != nil {
		return AdminGroup{}, fmt.Errorf("group last insert id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return AdminGroup{}, fmt.Errorf("commit create group: %w", err)
	}

	return AdminGroup{ID: groupID, Name: name, IsAdmin: false}, nil
}

// AddMembershipScoped adds a User to a Group after confirming both belong to the
// caller's tenant (Req 5.2, 32.6). A Group or User outside the tenant (or
// nonexistent) is rejected with ErrGroupNotAccessible / ErrUserNotAccessible
// before any write, which the REST layer maps to TENANT_ISOLATION (Req 1.6). The
// underlying insert is idempotent, so re-adding an existing membership succeeds.
func (s *UserGroupService) AddMembershipScoped(ctx context.Context, rc data.RequestContext, user data.UserID, group data.GroupID) error {
	if err := s.assertGroupInTenant(ctx, rc, group); err != nil {
		return err
	}
	if err := s.assertUserInTenant(ctx, rc, user); err != nil {
		return err
	}
	return s.AddMembership(ctx, user, group)
}

// RemoveMembershipScoped removes a User from a Group after confirming both
// belong to the caller's tenant (Req 5.2, 32.6). Cross-tenant references are
// rejected with the TENANT_ISOLATION sentinels before any write. The underlying
// RemoveMembership enforces the min-one-Group invariant (Req 4.1): removing a
// user's last Group returns ErrMinGroupMembership, which the REST layer maps to
// VALIDATION.
func (s *UserGroupService) RemoveMembershipScoped(ctx context.Context, rc data.RequestContext, user data.UserID, group data.GroupID) error {
	if err := s.assertGroupInTenant(ctx, rc, group); err != nil {
		return err
	}
	if err := s.assertUserInTenant(ctx, rc, user); err != nil {
		return err
	}
	return s.RemoveMembership(ctx, user, group)
}

// assertGroupInTenant confirms a Group exists and belongs to the caller's
// tenant. A Group in another tenant (or nonexistent) yields ErrGroupNotAccessible
// so a caller cannot probe for cross-tenant Groups (Req 1.6, 32.6).
func (s *UserGroupService) assertGroupInTenant(ctx context.Context, rc data.RequestContext, group data.GroupID) error {
	var tenantID int64
	err := s.conns.Central().QueryRowContext(ctx,
		`SELECT tenant_id FROM "GROUP" WHERE id = ?`,
		int64(group),
	).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && tenantID != int64(rc.TenantID())) {
		return ErrGroupNotAccessible
	}
	if err != nil {
		return fmt.Errorf("check group tenant: %w", err)
	}
	return nil
}

// assertUserInTenant confirms a User exists and belongs to the caller's tenant.
// A User in another tenant (or nonexistent) yields ErrUserNotAccessible (Req
// 1.6, 32.6).
func (s *UserGroupService) assertUserInTenant(ctx context.Context, rc data.RequestContext, user data.UserID) error {
	var tenantID int64
	err := s.conns.Central().QueryRowContext(ctx,
		`SELECT tenant_id FROM "USER" WHERE id = ?`,
		int64(user),
	).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && tenantID != int64(rc.TenantID())) {
		return ErrUserNotAccessible
	}
	if err != nil {
		return fmt.Errorf("check user tenant: %w", err)
	}
	return nil
}
