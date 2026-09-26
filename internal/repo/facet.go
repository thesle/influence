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

// This file implements the Facet repository: CRUD plus the cascading delete
// that keeps a Sphere's Facet forest and its contained Polygons consistent
// (design.md — "Sphere / Facet / Polygon Service"; Requirements 8.1–8.6).
//
// A Facet is a hierarchical category hub within a single Sphere (Req 8.1, 8.2).
// It carries an immutable Record_ID (Req 8.3, 15.4) and a 1–100 character name
// (Req 8.4, 8.5). Every operation runs against ONLY the caller's tenant DB via
// the embedded Base: a Sphere or Facet Record_ID that lives in another tenant
// is simply absent here and resolves to ErrNotAccessible (Req 1.6). Reparenting
// and hierarchy-cycle integrity (Req 9) are deliberately NOT handled here; they
// belong to a separate task.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// ErrFacetValidation is returned when a Facet create/update request violates a
// field rule — an empty name or a name longer than 100 characters (Req 8.5).
// Handlers map it to the VALIDATION error envelope. It is named distinctly from
// any Sphere-level validation sentinel so the two repositories can define their
// own rules without a redefinition clash.
var ErrFacetValidation = errors.New("facet validation failed")

// ErrParentSphereMismatch is returned when a create request names a parent
// Facet that lives in a different Sphere than the one the new Facet is being
// created in. A Facet's parent must be in the same Sphere (design.md — Facet
// hierarchy; Req 9.3), so a cross-Sphere parent is rejected up front and no row
// is written.
var ErrParentSphereMismatch = errors.New("parent facet is in a different sphere")

const (
	// facetNameMin and facetNameMax bound a Facet name at 1–100 characters
	// (Req 8.4, 8.5). Length is measured in runes so a multi-byte name is not
	// unfairly rejected by its byte count.
	facetNameMin = 1
	facetNameMax = 100
)

// Facet is a single category hub as returned by the repository. Record_ID is
// the external, immutable identifier; ParentRecordID is nil for a top-level
// Facet or the parent's Record_ID for a child. SphereRecordID names the Sphere
// the Facet belongs to.
type Facet struct {
	RecordID       string
	SphereRecordID string
	ParentRecordID *string
	Name           string
}

// FacetRepo is the tenant-scoped Facet repository. It embeds Base so every
// query is routed to the caller's own tenant DB and nowhere else.
type FacetRepo struct {
	Base
}

// NewFacetRepo constructs a FacetRepo over a ConnManager.
func NewFacetRepo(conns data.ConnManager) *FacetRepo {
	return &FacetRepo{Base: NewBase(conns)}
}

// Create stores a new Facet with the given name inside the Sphere identified by
// sphereRecordID, optionally under the parent Facet identified by
// parentFacetRecordID (nil for a top-level Facet). It validates the name is 1–100
// characters (Req 8.4, 8.5), resolves the Sphere and any parent within the
// caller's tenant (Req 1.6), assigns a fresh Record_ID (Req 8.3, 15.4), and
// returns the created Facet.
//
// A parent, when supplied, must belong to the same Sphere; otherwise the request
// is rejected with ErrParentSphereMismatch and no row is written.
func (r *FacetRepo) Create(ctx context.Context, rc data.RequestContext, sphereRecordID string, parentFacetRecordID *string, name string) (Facet, error) {
	if err := validateFacetName(name); err != nil {
		return Facet{}, err
	}

	sphereID, err := recordid.Parse(sphereRecordID)
	if err != nil {
		// An unparseable Sphere Record_ID cannot match any row; treat it as not
		// accessible rather than leaking a parse-vs-missing distinction.
		return Facet{}, ErrNotAccessible
	}

	sphereSurrogate, err := r.resolveRecord(ctx, rc, RecordSphere, sphereID)
	if err != nil {
		return Facet{}, err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return Facet{}, err
	}

	// Resolve the parent (if any) within the same tenant and confirm it lives in
	// the same Sphere. Both checks run before any write so a bad parent leaves
	// no partial state.
	var parentSurrogate sql.NullInt64
	if parentFacetRecordID != nil {
		pid, err := recordid.Parse(*parentFacetRecordID)
		if err != nil {
			return Facet{}, ErrNotAccessible
		}
		ps, err := r.resolveRecord(ctx, rc, RecordFacet, pid)
		if err != nil {
			return Facet{}, err
		}
		var parentSphere int64
		err = tdb.DB().QueryRowContext(ctx, `SELECT sphere_id FROM facet WHERE id = ?`, ps).Scan(&parentSphere)
		if err != nil {
			return Facet{}, fmt.Errorf("load parent facet sphere: %w", err)
		}
		if parentSphere != sphereSurrogate {
			return Facet{}, ErrParentSphereMismatch
		}
		parentSurrogate = sql.NullInt64{Int64: ps, Valid: true}
	}

	rid := recordid.New()
	_, err = tdb.DB().ExecContext(ctx,
		`INSERT INTO facet (record_id, sphere_id, parent_facet_id, name) VALUES (?, ?, ?, ?)`,
		rid.Canonical(), sphereSurrogate, parentSurrogate, name,
	)
	if err != nil {
		return Facet{}, fmt.Errorf("insert facet: %w", err)
	}

	return Facet{
		RecordID:       rid.Canonical(),
		SphereRecordID: sphereRecordID,
		ParentRecordID: parentFacetRecordID,
		Name:           name,
	}, nil
}

// Get returns the Facet identified by facetRecordID from the caller's tenant. A
// Record_ID absent from this tenant returns ErrNotAccessible (Req 1.6).
func (r *FacetRepo) Get(ctx context.Context, rc data.RequestContext, facetRecordID string) (Facet, error) {
	fid, err := recordid.Parse(facetRecordID)
	if err != nil {
		return Facet{}, ErrNotAccessible
	}
	if _, err := r.resolveRecord(ctx, rc, RecordFacet, fid); err != nil {
		return Facet{}, err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return Facet{}, err
	}

	// Join back to the Sphere and (optional) parent to return their external
	// Record_IDs rather than internal surrogate ids.
	const q = `
		SELECT f.record_id, s.record_id, p.record_id, f.name
		FROM facet f
		JOIN sphere s ON s.id = f.sphere_id
		LEFT JOIN facet p ON p.id = f.parent_facet_id
		WHERE f.record_id = ?`

	var facet Facet
	var parent sql.NullString
	err = tdb.DB().QueryRowContext(ctx, q, fid.Canonical()).
		Scan(&facet.RecordID, &facet.SphereRecordID, &parent, &facet.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return Facet{}, ErrNotAccessible
	}
	if err != nil {
		return Facet{}, fmt.Errorf("get facet: %w", err)
	}
	if parent.Valid {
		facet.ParentRecordID = &parent.String
	}
	return facet, nil
}

// List returns all Facets within the Sphere identified by sphereRecordID,
// ordered by name for a stable result. The Sphere is resolved within the
// caller's tenant first, so a Sphere in another tenant returns ErrNotAccessible.
func (r *FacetRepo) List(ctx context.Context, rc data.RequestContext, sphereRecordID string) ([]Facet, error) {
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
		SELECT f.record_id, s.record_id, p.record_id, f.name
		FROM facet f
		JOIN sphere s ON s.id = f.sphere_id
		LEFT JOIN facet p ON p.id = f.parent_facet_id
		WHERE f.sphere_id = ?
		ORDER BY f.name, f.id`

	rows, err := tdb.DB().QueryContext(ctx, q, sphereSurrogate)
	if err != nil {
		return nil, fmt.Errorf("list facets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Facet
	for rows.Next() {
		var facet Facet
		var parent sql.NullString
		if err := rows.Scan(&facet.RecordID, &facet.SphereRecordID, &parent, &facet.Name); err != nil {
			return nil, fmt.Errorf("scan facet: %w", err)
		}
		if parent.Valid {
			facet.ParentRecordID = &parent.String
		}
		out = append(out, facet)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate facets: %w", err)
	}
	return out, nil
}

// Delete removes the Facet identified by facetRecordID together with all of its
// descendant Facets and every Polygon contained in the deleted Facets, in a
// single transaction (Req 8.6). Either the whole subtree and its Polygons are
// gone or nothing changes.
//
// The tenant schema cascades parent_facet_id ON DELETE CASCADE (so deleting the
// root Facet removes descendant Facets) but declares polygon.facet_id ON DELETE
// SET NULL — a facet delete would otherwise merely orphan its Polygons. To honor
// "delete ... contained Polygons", this first computes the full descendant-Facet
// set with a recursive walk, deletes every Polygon assigned to any Facet in that
// set (their child Polygons cascade via parent_polygon_id), then deletes the root
// Facet so the Facet cascade clears the descendants.
func (r *FacetRepo) Delete(ctx context.Context, rc data.RequestContext, facetRecordID string) error {
	fid, err := recordid.Parse(facetRecordID)
	if err != nil {
		return ErrNotAccessible
	}
	rootSurrogate, err := r.resolveRecord(ctx, rc, RecordFacet, fid)
	if err != nil {
		return err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin facet delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Collect the target facet and every descendant via parent_facet_id. The
	// recursive CTE walks the subtree so contained Polygons at every level are
	// captured, not just the root's direct children.
	const subtreeQ = `
		WITH RECURSIVE subtree(id) AS (
			SELECT id FROM facet WHERE id = ?
			UNION ALL
			SELECT f.id FROM facet f JOIN subtree s ON f.parent_facet_id = s.id
		)
		SELECT id FROM subtree`

	rows, err := tx.QueryContext(ctx, subtreeQ, rootSurrogate)
	if err != nil {
		return fmt.Errorf("collect facet subtree: %w", err)
	}
	var facetIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan facet subtree: %w", err)
		}
		facetIDs = append(facetIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate facet subtree: %w", err)
	}
	_ = rows.Close()

	// Delete every Polygon contained in any Facet of the subtree. Child Polygons
	// (parent_polygon_id) and dependent rows (ciphertext, edit_lock, bookmark,
	// view_log) cascade via their own ON DELETE CASCADE.
	if len(facetIDs) > 0 {
		placeholders := strings.Repeat("?,", len(facetIDs))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(facetIDs))
		for i, id := range facetIDs {
			args[i] = id
		}
		delPolygons := fmt.Sprintf(`DELETE FROM polygon WHERE facet_id IN (%s)`, placeholders)
		if _, err := tx.ExecContext(ctx, delPolygons, args...); err != nil {
			return fmt.Errorf("delete contained polygons: %w", err)
		}
	}

	// Delete the root Facet; parent_facet_id ON DELETE CASCADE removes every
	// descendant Facet in the subtree.
	if _, err := tx.ExecContext(ctx, `DELETE FROM facet WHERE id = ?`, rootSurrogate); err != nil {
		return fmt.Errorf("delete facet: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit facet delete: %w", err)
	}
	return nil
}

// validateFacetName enforces the 1–100 character name rule (Req 8.4, 8.5),
// measuring length in runes. An empty or oversize name yields ErrFacetValidation.
func validateFacetName(name string) error {
	n := len([]rune(name))
	if n < facetNameMin {
		return fmt.Errorf("%w: name must not be empty", ErrFacetValidation)
	}
	if n > facetNameMax {
		return fmt.Errorf("%w: name must be at most %d characters", ErrFacetValidation, facetNameMax)
	}
	return nil
}
