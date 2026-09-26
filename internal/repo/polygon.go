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

package repo

// This file implements the Polygon repository: the Create/Get/List half of the
// content-unit service (design.md — "Sphere / Facet / Polygon Service";
// Requirements 10.1–10.7). A Polygon is the fundamental content unit within a
// Sphere and is always exactly one of four types (Req 10.1).
//
// Two invariants live here:
//
//   - Type validity. A Polygon type must be one of the four supported values —
//     Markdown_Page, Folder_Polygon, Tabular_Polygon, Whiteboard_Polygon
//     (Req 10.1, 10.2). A create request naming any other type is rejected with
//     ErrValidation before any row is written, so an invalid type creates
//     nothing (Req 10.3). ErrValidation is the shared repo validation sentinel
//     (defined in sphere.go) rather than a Polygon-specific one, so callers map
//     every service-layer field rejection to the same VALIDATION envelope.
//
//   - Atomic persistence. A Polygon is stored within its resolved Sphere in the
//     caller's tenant with a freshly minted, tenant-unique Record_ID (Req 10.4,
//     10.5). The whole create — resolving the Sphere/facet/parent and the insert
//     — runs inside one transaction, so if persistence fails nothing is written
//     and the target Sphere is left unchanged (Req 10.6).
//
// Folder containment (Req 10.7) is expressed through parent_polygon_id: a
// Folder_Polygon may contain other Polygons, including other Folder_Polygons.
// A create request may name a parent Folder_Polygon; the parent must itself be
// a folder in the same tenant.
//
// Like every repository here, PolygonRepo embeds Base and reaches the database
// only through the caller's own tenant handle, so all reads and writes are
// structurally scoped to rc.TenantID (Req 1.4, 1.5). A Sphere/facet/parent
// Record_ID that belongs to another tenant is simply absent here and resolves
// to ErrNotAccessible (Req 1.6).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// PolygonType is the stored type discriminator of a Polygon. Exactly four
// values are supported (Req 10.1); any other value is invalid (Req 10.3).
type PolygonType string

const (
	// PolygonMarkdown is a Markdown_Page — content authored in markdown with
	// @-commands (Req 10.1).
	PolygonMarkdown PolygonType = "markdown"
	// PolygonFolder is a Folder_Polygon — organizes other Polygons, including
	// other folders, via parent_polygon_id (Req 10.1, 10.7).
	PolygonFolder PolygonType = "folder"
	// PolygonTabular is a Tabular_Polygon — holds tabular data (Req 10.1).
	PolygonTabular PolygonType = "tabular"
	// PolygonWhiteboard is a Whiteboard_Polygon — holds freehand/chart content
	// (Req 10.1).
	PolygonWhiteboard PolygonType = "whiteboard"
)

// EditMode is the concurrent-editing mode a Polygon operates in. Every Polygon
// is in exactly one of two mutually exclusive modes, recorded in
// POLYGON.edit_mode (Req 25.1): collaborative editing or record locking. The
// modes are never mixed for the same Polygon — a Polygon is collaborative or it
// is record-locking, and the mode decides which concurrency path applies.
type EditMode string

const (
	// EditModeLocking is record-locking mode: a single editor holds an
	// EDIT_LOCK and other users are prevented from saving until it releases
	// (Req 25.3–25.5). It is the schema default (tenant_schema.go).
	EditModeLocking EditMode = "locking"
	// EditModeCollaborative is collaborative mode: edits are merged live across
	// all connected editors over the WebSocket hub (Req 25.2).
	EditModeCollaborative EditMode = "collaborative"
)

// validEditModes is the closed set of supported edit modes (Req 25.1). Any
// stored value outside this set is not a valid mode.
var validEditModes = map[EditMode]struct{}{
	EditModeLocking:       {},
	EditModeCollaborative: {},
}

// Valid reports whether m is one of the two supported edit modes (Req 25.1).
func (m EditMode) Valid() bool {
	_, ok := validEditModes[m]
	return ok
}

// validPolygonTypes is the closed set of supported Polygon types (Req 10.1).
// Membership is the single source of truth for type validation: a value not in
// this set is rejected without creating a Polygon (Req 10.3).
var validPolygonTypes = map[PolygonType]struct{}{
	PolygonMarkdown:   {},
	PolygonFolder:     {},
	PolygonTabular:    {},
	PolygonWhiteboard: {},
}

// Polygon is a content unit as returned by the repository. Record_ID is the
// external, immutable identifier (Req 10.4, 15.4); SphereRecordID names the
// owning Sphere (Req 10.5). FacetRecordID is set when the Polygon is filed under
// a Facet; ParentRecordID is set when the Polygon sits inside a Folder_Polygon
// (Req 10.7). Both are nil otherwise.
type Polygon struct {
	RecordID       string
	SphereRecordID string
	FacetRecordID  *string
	ParentRecordID *string
	Type           PolygonType
	Content        string
	// EditMode is the Polygon's concurrent-editing mode (Req 25.1): exactly one
	// of collaborative or record-locking. It decides which concurrency path the
	// WebSocket hub applies to this Polygon.
	EditMode     EditMode
	AuthorUserID int64
	CreatedAt    string
	UpdatedAt    string
}

// PolygonRepo is the tenant-scoped Polygon repository. It embeds Base so every
// query is routed to the caller's own tenant DB and nowhere else.
type PolygonRepo struct {
	Base
}

// NewPolygonRepo constructs a PolygonRepo over a ConnManager.
func NewPolygonRepo(conns data.ConnManager) *PolygonRepo {
	return &PolygonRepo{Base: NewBase(conns)}
}

// CreateOptions carries the optional placement of a new Polygon. Both fields are
// nil by default: FacetRecordID files the Polygon under a Facet, ParentRecordID
// nests it inside a Folder_Polygon (Req 10.7). They are independent — a Polygon
// may sit under a Facet, inside a folder, both, or neither.
type CreateOptions struct {
	// FacetRecordID, when non-nil, is the Record_ID of the Facet the new
	// Polygon is categorized under. It must resolve within the caller's tenant.
	FacetRecordID *string
	// ParentRecordID, when non-nil, is the Record_ID of the Folder_Polygon that
	// contains the new Polygon (Req 10.7). It must resolve within the caller's
	// tenant and must itself be a Folder_Polygon.
	ParentRecordID *string
}

// Create validates and stores a new Polygon of the given type inside the Sphere
// identified by sphereRecordID, authored by authorUserID, applying any optional
// Facet/parent placement in opts.
//
// The type must be one of the four supported values; otherwise Create returns
// ErrValidation and writes nothing (Req 10.1, 10.3). The Sphere is resolved
// within the caller's tenant (a Sphere in another tenant yields ErrNotAccessible
// — Req 1.6), the Polygon is stored in that Sphere (Req 10.5) with a fresh,
// tenant-unique Record_ID (Req 10.4), and the whole operation runs in one
// transaction so a persistence failure leaves the Sphere unchanged (Req 10.6).
//
// When opts.ParentRecordID is supplied it must name a Folder_Polygon in the same
// tenant; a non-folder parent is rejected with ErrValidation (folders are the
// only Polygons that may contain others — Req 10.7).
func (r *PolygonRepo) Create(ctx context.Context, rc data.RequestContext, sphereRecordID string, typ PolygonType, authorUserID int64, opts CreateOptions) (Polygon, error) {
	// Type validity first — a rejected type must create nothing (Req 10.3).
	if _, ok := validPolygonTypes[typ]; !ok {
		return Polygon{}, fmt.Errorf("%w: polygon type %q is not one of markdown, folder, tabular, whiteboard", ErrValidation, typ)
	}

	sphereID, err := recordid.Parse(sphereRecordID)
	if err != nil {
		// An unparseable Sphere Record_ID cannot match any row; treat it as not
		// accessible rather than leaking a parse-vs-missing distinction.
		return Polygon{}, ErrNotAccessible
	}
	sphereSurrogate, err := r.resolveRecord(ctx, rc, RecordSphere, sphereID)
	if err != nil {
		return Polygon{}, err
	}

	// Resolve optional Facet placement within the same tenant.
	var facetSurrogate sql.NullInt64
	if opts.FacetRecordID != nil {
		fid, err := recordid.Parse(*opts.FacetRecordID)
		if err != nil {
			return Polygon{}, ErrNotAccessible
		}
		fs, err := r.resolveRecord(ctx, rc, RecordFacet, fid)
		if err != nil {
			return Polygon{}, err
		}
		facetSurrogate = sql.NullInt64{Int64: fs, Valid: true}
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return Polygon{}, err
	}

	// Resolve the optional parent folder within the same tenant and confirm it
	// is itself a Folder_Polygon — only folders may contain other Polygons
	// (Req 10.7). This runs before any write so a bad parent leaves no state.
	var parentSurrogate sql.NullInt64
	if opts.ParentRecordID != nil {
		pid, err := recordid.Parse(*opts.ParentRecordID)
		if err != nil {
			return Polygon{}, ErrNotAccessible
		}
		ps, err := r.resolveRecord(ctx, rc, RecordPolygon, pid)
		if err != nil {
			return Polygon{}, err
		}
		var parentType string
		if err := tdb.DB().QueryRowContext(ctx, `SELECT type FROM polygon WHERE id = ?`, ps).Scan(&parentType); err != nil {
			return Polygon{}, fmt.Errorf("load parent polygon type: %w", err)
		}
		if PolygonType(parentType) != PolygonFolder {
			return Polygon{}, fmt.Errorf("%w: parent polygon must be a folder to contain other polygons", ErrValidation)
		}
		parentSurrogate = sql.NullInt64{Int64: ps, Valid: true}
	}

	// Persist inside a transaction: either the Polygon row is committed or the
	// Sphere is left entirely unchanged (Req 10.6).
	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return Polygon{}, fmt.Errorf("begin create polygon: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rid := recordid.New()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO polygon (record_id, sphere_id, facet_id, parent_polygon_id, type, author_user_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rid.Canonical(), sphereSurrogate, facetSurrogate, parentSurrogate, string(typ), authorUserID, now, now,
	); err != nil {
		return Polygon{}, fmt.Errorf("insert polygon: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Polygon{}, fmt.Errorf("commit create polygon: %w", err)
	}

	return Polygon{
		RecordID:       rid.Canonical(),
		SphereRecordID: sphereRecordID,
		FacetRecordID:  opts.FacetRecordID,
		ParentRecordID: opts.ParentRecordID,
		Type:           typ,
		Content:        "",
		// The insert omits edit_mode, so the row takes the schema default of
		// record-locking (tenant_schema.go, Req 25.1). Reflect that here.
		EditMode:     EditModeLocking,
		AuthorUserID: authorUserID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// Touch records an edit of the Polygon identified by polygonRecordID by
// refreshing its last-edit timestamp (updated_at) to the current time as an
// ISO 8601 timestamp in UTC (Req 18.2). It changes only updated_at — the
// author and creation timestamp recorded at Create (Req 18.1) are left intact.
//
// Like every operation here it is scoped to the caller's own tenant: the
// Record_ID is resolved within rc.TenantID first, so a Polygon that lives in
// another tenant is absent and returns ErrNotAccessible with nothing changed
// (Req 1.6). It returns the refreshed Polygon so callers can render the updated
// footer without a second read.
func (r *PolygonRepo) Touch(ctx context.Context, rc data.RequestContext, polygonRecordID string) (Polygon, error) {
	pid, err := recordid.Parse(polygonRecordID)
	if err != nil {
		return Polygon{}, ErrNotAccessible
	}
	if _, err := r.resolveRecord(ctx, rc, RecordPolygon, pid); err != nil {
		return Polygon{}, err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return Polygon{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tdb.DB().ExecContext(ctx,
		`UPDATE polygon SET updated_at = ? WHERE record_id = ?`,
		now, pid.Canonical(),
	); err != nil {
		return Polygon{}, fmt.Errorf("touch polygon: %w", err)
	}

	return r.Get(ctx, rc, polygonRecordID)
}

// Get returns the Polygon identified by polygonRecordID from the caller's
// tenant. A Record_ID absent from this tenant returns ErrNotAccessible (Req 1.6).
func (r *PolygonRepo) Get(ctx context.Context, rc data.RequestContext, polygonRecordID string) (Polygon, error) {
	pid, err := recordid.Parse(polygonRecordID)
	if err != nil {
		return Polygon{}, ErrNotAccessible
	}
	if _, err := r.resolveRecord(ctx, rc, RecordPolygon, pid); err != nil {
		return Polygon{}, err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return Polygon{}, err
	}

	// Join back to the Sphere and (optional) Facet/parent to return their
	// external Record_IDs rather than internal surrogate ids.
	const q = `
		SELECT p.record_id, s.record_id, f.record_id, pp.record_id,
		       p.type, p.content, p.edit_mode, p.author_user_id, p.created_at, p.updated_at
		FROM polygon p
		JOIN sphere s ON s.id = p.sphere_id
		LEFT JOIN facet f ON f.id = p.facet_id
		LEFT JOIN polygon pp ON pp.id = p.parent_polygon_id
		WHERE p.record_id = ?`

	var (
		poly     Polygon
		facet    sql.NullString
		parent   sql.NullString
		typ      string
		editMode string
	)
	err = tdb.DB().QueryRowContext(ctx, q, pid.Canonical()).Scan(
		&poly.RecordID, &poly.SphereRecordID, &facet, &parent,
		&typ, &poly.Content, &editMode, &poly.AuthorUserID, &poly.CreatedAt, &poly.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Polygon{}, ErrNotAccessible
	}
	if err != nil {
		return Polygon{}, fmt.Errorf("get polygon: %w", err)
	}
	poly.Type = PolygonType(typ)
	poly.EditMode = EditMode(editMode)
	if facet.Valid {
		poly.FacetRecordID = &facet.String
	}
	if parent.Valid {
		poly.ParentRecordID = &parent.String
	}
	return poly, nil
}

// Mode returns the edit mode of the Polygon identified by polygonRecordID from
// the caller's tenant — exactly one of collaborative or record-locking
// (Req 25.1). It is the resolution the WebSocket hub uses to decide which
// concurrency path a per-Polygon room follows, and it reads only the small
// edit_mode column rather than the full content.
//
// Like every operation here it is tenant-scoped: a Record_ID absent from the
// caller's tenant returns ErrNotAccessible (Req 1.6). A stored value outside
// the supported set is returned as-is; callers may check EditMode.Valid.
func (r *PolygonRepo) Mode(ctx context.Context, rc data.RequestContext, polygonRecordID string) (EditMode, error) {
	pid, err := recordid.Parse(polygonRecordID)
	if err != nil {
		return "", ErrNotAccessible
	}
	if _, err := r.resolveRecord(ctx, rc, RecordPolygon, pid); err != nil {
		return "", err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return "", err
	}

	var mode string
	err = tdb.DB().QueryRowContext(ctx,
		`SELECT edit_mode FROM polygon WHERE record_id = ?`, pid.Canonical(),
	).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotAccessible
	}
	if err != nil {
		return "", fmt.Errorf("get polygon edit mode: %w", err)
	}
	return EditMode(mode), nil
}

// List returns all Polygons within the Sphere identified by sphereRecordID,
// ordered by creation time then Record_ID for a stable result. The Sphere is
// resolved within the caller's tenant first, so a Sphere in another tenant
// returns ErrNotAccessible (Req 1.6).
func (r *PolygonRepo) List(ctx context.Context, rc data.RequestContext, sphereRecordID string) ([]Polygon, error) {
	sid, err := recordid.Parse(sphereRecordID)
	if err != nil {
		return nil, ErrNotAccessible
	}
	sphereSurrogate, err := r.resolveRecord(ctx, rc, RecordSphere, sid)
	if err != nil {
		return nil, err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return nil, err
	}

	const q = `
		SELECT p.record_id, s.record_id, f.record_id, pp.record_id,
		       p.type, p.content, p.edit_mode, p.author_user_id, p.created_at, p.updated_at
		FROM polygon p
		JOIN sphere s ON s.id = p.sphere_id
		LEFT JOIN facet f ON f.id = p.facet_id
		LEFT JOIN polygon pp ON pp.id = p.parent_polygon_id
		WHERE p.sphere_id = ?
		ORDER BY p.created_at, p.record_id`

	rows, err := tdb.DB().QueryContext(ctx, q, sphereSurrogate)
	if err != nil {
		return nil, fmt.Errorf("list polygons: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Polygon
	for rows.Next() {
		var (
			poly     Polygon
			facet    sql.NullString
			parent   sql.NullString
			typ      string
			editMode string
		)
		if err := rows.Scan(
			&poly.RecordID, &poly.SphereRecordID, &facet, &parent,
			&typ, &poly.Content, &editMode, &poly.AuthorUserID, &poly.CreatedAt, &poly.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan polygon: %w", err)
		}
		poly.Type = PolygonType(typ)
		poly.EditMode = EditMode(editMode)
		if facet.Valid {
			poly.FacetRecordID = &facet.String
		}
		if parent.Valid {
			poly.ParentRecordID = &parent.String
		}
		out = append(out, poly)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate polygons: %w", err)
	}
	return out, nil
}
