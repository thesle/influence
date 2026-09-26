package service

// The per-Sphere Group grant half of Requirement 5.3 / 32.4: an Admin_Group
// member lists the Sphere grants in their Tenant and grants or revokes a Group's
// access (level + Reveal) to a Sphere. These are the operations the admin
// sphere-access routes (GET/PUT/DELETE /api/admin/sphere-access) delegate to,
// keeping those handlers thin.
//
// Where the state lives (and why scoping is split). A SPHERE_GRANT row lives in
// the tenant's own database keyed on the internal sphere surrogate id and the
// Central GROUP id (design.md — "SPHERE_GRANT { sphere_id, group_id, access,
// reveal }"). The Sphere it references is therefore tenant-local and is named to
// the API by its immutable Record_ID, while the Group is platform-wide and lives
// in the Central_Directory. So a grant/revoke must resolve both halves within
// the caller's tenant:
//
//   - The Sphere Record_ID is looked up in the caller's tenant database; a
//     Record_ID absent from that tenant (nonexistent or owned by another tenant)
//     is indistinguishable from a missing one and yields ErrSphereNotAccessible
//     (Req 1.6), which the REST layer maps to TENANT_ISOLATION.
//
//   - The Group id is confirmed to belong to the caller's tenant via the same
//     assertGroupInTenant check the membership routes use; a cross-tenant Group
//     yields ErrGroupNotAccessible → TENANT_ISOLATION (Req 32.6).
//
// The underlying GrantSphere/RevokeSphere already resolve the caller's tenant
// from the RequestContext and never touch another tenant's database, so once the
// sphere surrogate is resolved the write is confined to the caller's tenant by
// construction.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// ErrSphereNotAccessible is returned when a grant/revoke names a Sphere that is
// not in the caller's tenant (or does not exist). Like ErrUserNotAccessible /
// ErrGroupNotAccessible the two cases are deliberately indistinguishable so a
// caller cannot probe for the existence of Spheres in other tenants (Req 1.6,
// 32.6). The REST layer maps it to the TENANT_ISOLATION envelope.
var ErrSphereNotAccessible = errors.New("sphere not accessible")

// SphereGrantEntry is the projection of a SPHERE_GRANT row the admin sphere-
// access listing returns. It carries the external Sphere Record_ID (never the
// internal surrogate), the Central GROUP id the grant is for, and the access
// level + Reveal flag the grant confers (Req 5.3, 32.4).
type SphereGrantEntry struct {
	// SphereRecordID is the immutable external Record_ID of the granted Sphere.
	SphereRecordID string
	// Group is the Central_Directory GROUP id the grant applies to.
	Group data.GroupID
	// Access is the level the grant confers: read or write.
	Access data.AccessLevel
	// Reveal reports whether the grant carries Reveal_Permission (Req 16.6).
	Reveal bool
}

// ListSphereGrants returns every SPHERE_GRANT in the caller's tenant, joined to
// the Sphere's Record_ID so the listing speaks external ids only (Req 5.3,
// 32.4). The tenant is resolved from the RequestContext, never from client
// input, so the result can only ever contain the caller's own tenant's grants
// (Req 32.6). The order is stable: by Sphere Record_ID then Group id.
func (s *UserGroupService) ListSphereGrants(ctx context.Context, rc data.RequestContext) ([]SphereGrantEntry, error) {
	tdb, err := s.conns.Tenant(ctx, rc)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant for grants list: %w", err)
	}

	rows, err := tdb.DB().QueryContext(ctx,
		`SELECT sphere.record_id, sphere_grant.group_id, sphere_grant.access, sphere_grant.reveal
		   FROM sphere_grant
		   JOIN sphere ON sphere.id = sphere_grant.sphere_id
		  ORDER BY sphere.record_id, sphere_grant.group_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list sphere grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SphereGrantEntry
	for rows.Next() {
		var (
			recordID string
			groupID  int64
			access   string
			reveal   int
		)
		if err := rows.Scan(&recordID, &groupID, &access, &reveal); err != nil {
			return nil, fmt.Errorf("scan sphere grant: %w", err)
		}
		out = append(out, SphereGrantEntry{
			SphereRecordID: recordID,
			Group:          data.GroupID(groupID),
			Access:         accessFromText(access),
			Reveal:         reveal != 0,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sphere grants: %w", err)
	}
	return out, nil
}

// GrantSphereByRecordID grants a Group access to the Sphere named by its
// external Record_ID at the given level with the given Reveal flag, in the
// caller's tenant (Req 5.3, 32.4, 4.2, 4.3, 4.4). Both halves are confined to
// the caller's tenant first: the Group must belong to it (else
// ErrGroupNotAccessible) and the Sphere Record_ID must resolve within it (else
// ErrSphereNotAccessible), both mapping to TENANT_ISOLATION (Req 1.6, 32.6). An
// access level other than read or write is ErrInvalidAccessLevel (VALIDATION).
// The resolved surrogate is handed to GrantSphere, which upserts the grant.
func (s *UserGroupService) GrantSphereByRecordID(ctx context.Context, rc data.RequestContext, sphere recordid.ID, group data.GroupID, access data.AccessLevel, reveal bool) error {
	if access != data.AccessRead && access != data.AccessWrite {
		return ErrInvalidAccessLevel
	}
	if err := s.assertGroupInTenant(ctx, rc, group); err != nil {
		return err
	}
	surrogate, err := s.resolveSphereSurrogate(ctx, rc, sphere)
	if err != nil {
		return err
	}
	return s.GrantSphere(ctx, rc, surrogate, group, access, reveal)
}

// RevokeSphereByRecordID removes a Group's grant on the Sphere named by its
// external Record_ID, in the caller's tenant (Req 5.3, 32.4, 4.6). The Group and
// Sphere are confined to the caller's tenant first (ErrGroupNotAccessible /
// ErrSphereNotAccessible → TENANT_ISOLATION). Revoking a grant that does not
// exist is a no-op success, delegated to RevokeSphere.
func (s *UserGroupService) RevokeSphereByRecordID(ctx context.Context, rc data.RequestContext, sphere recordid.ID, group data.GroupID) error {
	if err := s.assertGroupInTenant(ctx, rc, group); err != nil {
		return err
	}
	surrogate, err := s.resolveSphereSurrogate(ctx, rc, sphere)
	if err != nil {
		return err
	}
	return s.RevokeSphere(ctx, rc, surrogate, group)
}

// resolveSphereSurrogate maps a Sphere Record_ID to its internal surrogate id
// within the caller's tenant. A Record_ID absent from the caller's tenant
// database (nonexistent or owned by another tenant) yields ErrSphereNotAccessible
// so a caller cannot probe for cross-tenant Spheres (Req 1.6, 32.6). The lookup
// runs against the tenant resolved from the RequestContext, never an arbitrary
// tenant.
func (s *UserGroupService) resolveSphereSurrogate(ctx context.Context, rc data.RequestContext, sphere recordid.ID) (data.SphereID, error) {
	tdb, err := s.conns.Tenant(ctx, rc)
	if err != nil {
		return 0, fmt.Errorf("resolve tenant for sphere lookup: %w", err)
	}

	var surrogate int64
	err = tdb.DB().QueryRowContext(ctx,
		`SELECT id FROM sphere WHERE record_id = ?`, sphere.Canonical(),
	).Scan(&surrogate)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSphereNotAccessible
	}
	if err != nil {
		return 0, fmt.Errorf("resolve sphere surrogate: %w", err)
	}
	return data.SphereID(surrogate), nil
}
