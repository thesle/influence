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

package service

// The user/group management service owns the write-side of Requirement 4: the
// operations an administrator performs to shape who belongs to which Group and
// which Spheres a Group can reach. It is the counterpart to the read-only
// PolicyEngine (policy.go): this service mutates the membership and grant tables
// the PolicyEngine later reduces to an effective decision.
//
// Where the state lives (and why it is split across the two databases):
//
//   - GROUP_MEMBERSHIP and GROUP live in the Central_Directory alongside USER,
//     because identity is platform-wide (Requirement 1.1). Membership changes
//     therefore run against conns.Central().
//
//   - SPHERE_GRANT lives in the tenant's own database (keyed sphere_id,
//     group_id, access, reveal) so an exported tenant carries its own access map
//     (Requirement 2.1). Grant changes run against the caller's TenantDB
//     resolved from the RequestContext — never an arbitrary tenant.
//
// Two invariants shape the API:
//
//   - Every User must belong to at least one Group at all times (Requirement
//     4.1). AddMembership only grows the set, so it can never break the rule;
//     RemoveMembership refuses the removal that would drop a user to zero and
//     returns ErrMinGroupMembership, leaving memberships unchanged.
//
//   - "Grant a Group access to a Sphere applies to every current member"
//     (Requirement 4.3) and "revoke on grant loss unless retained via another
//     Group" (Requirement 4.6) are satisfied structurally, not by fanning out
//     per-user rows. A grant is stored once per (Sphere, Group); a user's
//     effective access is recomputed from their current memberships unioned
//     across their Groups' grants. Adding or removing a grant, or a membership,
//     therefore changes every affected member's effective access immediately and
//     a member who still shares another granting Group keeps access — no
//     revocation bookkeeping is required. EffectiveAccess exposes that
//     recomputation and delegates the union to the PolicyEngine so the "most
//     permissive across Groups / reveal iff any Group has reveal" rules
//     (Req 4.3, 4.4) live in exactly one place.

import (
	"context"
	"errors"
	"fmt"

	"github.com/influence/influence/internal/data"
)

// ErrMinGroupMembership is returned when a membership removal would leave a User
// with zero Group memberships. The operation is rejected and the user's
// memberships are left unchanged (Requirement 4.1).
var ErrMinGroupMembership = errors.New("at least one group membership is required")

// ErrInvalidAccessLevel is returned when a grant is requested with an access
// level that is neither read nor write. A Sphere grant must confer a concrete
// level (Requirement 4.2, 4.3); AccessNone is not a grant, it is the absence of
// one, so callers revoke instead of granting AccessNone.
var ErrInvalidAccessLevel = errors.New("grant access level must be read or write")

// UserGroupService performs the administrator operations of Requirement 4:
// managing Group memberships (Central_Directory) and per-Sphere Group grants
// (tenant database). It holds the ConnManager so membership changes reach the
// shared Central handle and grant changes reach only the caller's tenant, plus
// the PolicyEngine so effective-access queries reuse the single audited union.
type UserGroupService struct {
	conns  data.ConnManager
	policy PolicyEngine
}

// NewUserGroupService constructs a UserGroupService over the connection manager.
// It uses the default PolicyEngine for effective-access computation so the union
// rules are not duplicated here.
func NewUserGroupService(conns data.ConnManager) *UserGroupService {
	return &UserGroupService{conns: conns, policy: NewPolicyEngine()}
}

// AddMembership adds a User to a Group. It is idempotent: re-adding an existing
// membership is a no-op and not an error. Because it only ever grows a user's
// Group set it can never violate the >=1 invariant (Requirement 4.1).
//
// The membership edge lives in the Central_Directory (Requirement 1.1). The
// insert relies on the GROUP_MEMBERSHIP composite primary key (user_id,
// group_id) to make a duplicate a no-op via INSERT OR IGNORE.
func (s *UserGroupService) AddMembership(ctx context.Context, user data.UserID, group data.GroupID) error {
	central := s.conns.Central()
	if _, err := central.ExecContext(ctx,
		`INSERT OR IGNORE INTO "GROUP_MEMBERSHIP"(user_id, group_id) VALUES(?, ?)`,
		int64(user), int64(group),
	); err != nil {
		return fmt.Errorf("add membership: %w", err)
	}
	return nil
}

// RemoveMembership removes a User from a Group while enforcing the invariant
// that a User always belongs to at least one Group (Requirement 4.1).
//
// The check-and-delete runs in a single transaction so a concurrent removal
// cannot race the count and drop a user to zero: the current membership count is
// read, and if removing this membership would leave zero the transaction is
// rolled back and ErrMinGroupMembership is returned with memberships unchanged.
// Removing a membership the user does not have is a no-op success (the count of
// remaining memberships is unaffected), which keeps the operation idempotent.
func (s *UserGroupService) RemoveMembership(ctx context.Context, user data.UserID, group data.GroupID) error {
	central := s.conns.Central()

	tx, err := central.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin membership tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Does the membership being removed actually exist? If not, there is
	// nothing to remove and the invariant cannot be affected, so succeed
	// without touching the table.
	var has int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "GROUP_MEMBERSHIP" WHERE user_id = ? AND group_id = ?`,
		int64(user), int64(group),
	).Scan(&has); err != nil {
		return fmt.Errorf("check membership: %w", err)
	}
	if has == 0 {
		return nil
	}

	// The user currently has this membership. Reject the removal if it is the
	// user's last one (Req 4.1): total memberships of 1 means removing it drops
	// the user to zero.
	var total int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "GROUP_MEMBERSHIP" WHERE user_id = ?`,
		int64(user),
	).Scan(&total); err != nil {
		return fmt.Errorf("count memberships: %w", err)
	}
	if total <= 1 {
		return ErrMinGroupMembership
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM "GROUP_MEMBERSHIP" WHERE user_id = ? AND group_id = ?`,
		int64(user), int64(group),
	); err != nil {
		return fmt.Errorf("remove membership: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit membership removal: %w", err)
	}
	return nil
}

// Memberships returns the Groups a User currently belongs to, read from the
// Central_Directory. The order is unspecified.
func (s *UserGroupService) Memberships(ctx context.Context, user data.UserID) ([]data.GroupID, error) {
	rows, err := s.conns.Central().QueryContext(ctx,
		`SELECT group_id FROM "GROUP_MEMBERSHIP" WHERE user_id = ?`,
		int64(user),
	)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var groups []data.GroupID
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		groups = append(groups, data.GroupID(g))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memberships: %w", err)
	}
	return groups, nil
}

// GrantSphere grants a Group access to a Sphere at the given level with the
// given Reveal flag, in the caller's tenant (Requirements 4.2, 4.3, 4.4). The
// grant is stored once per (Sphere, Group); every current and future member of
// the Group derives that access from it, so there is no per-member fan-out.
//
// Re-granting an existing (Sphere, Group) pair updates the access level and
// reveal flag (upsert), which is how an administrator changes an existing
// grant. access must be AccessRead or AccessWrite; AccessNone is rejected with
// ErrInvalidAccessLevel because "no access" is expressed by revoking, not by a
// grant row.
func (s *UserGroupService) GrantSphere(ctx context.Context, rc data.RequestContext, sphere data.SphereID, group data.GroupID, access data.AccessLevel, reveal bool) error {
	if access != data.AccessRead && access != data.AccessWrite {
		return ErrInvalidAccessLevel
	}

	tdb, err := s.conns.Tenant(ctx, rc)
	if err != nil {
		return fmt.Errorf("resolve tenant for grant: %w", err)
	}

	if _, err := tdb.DB().ExecContext(ctx,
		`INSERT INTO sphere_grant(sphere_id, group_id, access, reveal) VALUES(?, ?, ?, ?)
		 ON CONFLICT(sphere_id, group_id)
		 DO UPDATE SET access = excluded.access, reveal = excluded.reveal`,
		int64(sphere), int64(group), accessToText(access), boolToInt(reveal),
	); err != nil {
		return fmt.Errorf("grant sphere access: %w", err)
	}
	return nil
}

// RevokeSphere removes a Group's grant on a Sphere in the caller's tenant
// (Requirement 4.6). Because effective access is recomputed from the surviving
// grants, this immediately withdraws the Sphere from every member of the Group
// unless the member retains access through another Group — no per-member cleanup
// is needed. Revoking a grant that does not exist is a no-op success.
func (s *UserGroupService) RevokeSphere(ctx context.Context, rc data.RequestContext, sphere data.SphereID, group data.GroupID) error {
	tdb, err := s.conns.Tenant(ctx, rc)
	if err != nil {
		return fmt.Errorf("resolve tenant for revoke: %w", err)
	}

	if _, err := tdb.DB().ExecContext(ctx,
		`DELETE FROM sphere_grant WHERE sphere_id = ? AND group_id = ?`,
		int64(sphere), int64(group),
	); err != nil {
		return fmt.Errorf("revoke sphere access: %w", err)
	}
	return nil
}

// EffectiveAccess computes a User's current effective access to a Sphere by
// loading the user's Group memberships (Central) and the grants those Groups
// hold on the Sphere (tenant), then delegating the union to the PolicyEngine
// (Requirements 4.3, 4.4, 4.5, 4.6). It is the read counterpart that proves the
// grant/membership model behaves correctly: it reflects the union across the
// user's current Groups, so a grant applies to every current member (4.3) and a
// revoked grant still leaves access intact when another Group grants it (4.6).
//
// admin and deactivated let a caller fold in the Admin_Group implicit grant and
// the deactivated lockout; when both are false this returns the plain
// group-derived access. It returns the effective SphereGrant (level + reveal).
func (s *UserGroupService) EffectiveAccess(ctx context.Context, rc data.RequestContext, user data.UserID, sphere data.SphereID, admin, deactivated bool) (data.SphereGrant, error) {
	groups, err := s.Memberships(ctx, user)
	if err != nil {
		return data.SphereGrant{}, err
	}

	grants, err := s.grantsForGroups(ctx, rc, groups)
	if err != nil {
		return data.SphereGrant{}, err
	}

	authz := AuthzData{Admin: admin, Deactivated: deactivated, Grants: grants}
	access, err := s.policy.CanAccessSphere(rc, authz, sphere)
	if err != nil {
		return data.SphereGrant{}, err
	}
	reveal := s.policy.CanReveal(rc, authz, sphere)
	return data.SphereGrant{Access: access, Reveal: reveal}, nil
}

// grantsForGroups loads every SPHERE_GRANT row held by the given Groups from the
// caller's tenant database and returns them as PolicyEngine GroupGrants. An
// empty group set yields no grants without touching the database.
func (s *UserGroupService) grantsForGroups(ctx context.Context, rc data.RequestContext, groups []data.GroupID) ([]GroupGrant, error) {
	if len(groups) == 0 {
		return nil, nil
	}

	tdb, err := s.conns.Tenant(ctx, rc)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant for grants: %w", err)
	}

	// Build an IN (?, ?, ...) list; the placeholders are generated from the
	// group count and every group id is bound as a parameter, so no client
	// value is interpolated into the SQL text.
	placeholders := make([]byte, 0, len(groups)*2)
	args := make([]any, 0, len(groups))
	for i, g := range groups {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, int64(g))
	}

	query := fmt.Sprintf(
		`SELECT sphere_id, group_id, access, reveal FROM sphere_grant WHERE group_id IN (%s)`,
		placeholders,
	)
	rows, err := tdb.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load group grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []GroupGrant
	for rows.Next() {
		var (
			sphereID int64
			groupID  int64
			access   string
			reveal   int
		)
		if err := rows.Scan(&sphereID, &groupID, &access, &reveal); err != nil {
			return nil, fmt.Errorf("scan grant: %w", err)
		}
		out = append(out, GroupGrant{
			Group:  data.GroupID(groupID),
			Sphere: data.SphereID(sphereID),
			Access: accessFromText(access),
			Reveal: reveal != 0,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate grants: %w", err)
	}
	return out, nil
}

// accessToText maps an AccessLevel to the text stored in SPHERE_GRANT.access.
// Only read and write are valid grant levels; callers validate before storing.
func accessToText(a data.AccessLevel) string {
	switch a {
	case data.AccessWrite:
		return "write"
	default:
		return "read"
	}
}

// accessFromText maps a stored SPHERE_GRANT.access value back to an AccessLevel.
// An unrecognized value fails closed to AccessNone so a malformed row never
// silently grants access.
func accessFromText(s string) data.AccessLevel {
	switch s {
	case "write":
		return data.AccessWrite
	case "read":
		return data.AccessRead
	default:
		return data.AccessNone
	}
}

// boolToInt maps a Go bool to the 0/1 integer SQLite stores for the reveal flag.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
