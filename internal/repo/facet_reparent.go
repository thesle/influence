package repo

// This file implements Facet reparenting with hierarchy-integrity enforcement
// (design.md — "Hierarchy integrity (Req 9)"; Requirements 9.1, 9.2, 9.3).
//
// A Sphere's Facets form a forest: every Facet has at most one parent and no
// Facet may be its own ancestor (Req 9.2). Reparent moves a Facet under a new
// parent (or to top level) while preserving those invariants. It rejects, and
// leaves the hierarchy untouched, when the proposed parent is the Facet itself
// or one of its descendants (would create a cycle — Req 9.1) or lives in a
// different Sphere (Req 9.3). All checks run before any write so a rejected
// reparent changes nothing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// ErrHierarchy is returned when a reparent would violate the Facet forest's
// integrity: the proposed parent is the Facet itself, a descendant of the Facet
// (which would form a cycle), or a Facet in a different Sphere (Req 9.1, 9.3).
// Handlers map it to the HIERARCHY (409) error envelope. The hierarchy is left
// unchanged whenever this is returned.
var ErrHierarchy = errors.New("facet hierarchy integrity violation")

// Reparent sets the parent of the Facet identified by facetRecordID. A nil
// newParentRecordID makes the Facet top-level; otherwise its parent becomes the
// Facet identified by newParentRecordID.
//
// The move is rejected with ErrHierarchy — leaving the hierarchy unchanged — if
// the proposed parent is the Facet itself, a descendant of the Facet (Req 9.1),
// or a Facet in a different Sphere (Req 9.3). Both Facets are resolved within
// the caller's tenant first, so a Record_ID from another tenant resolves to
// ErrNotAccessible (Req 1.6). All validation runs before the single UPDATE, so
// the resulting forest keeps every Facet with at most one parent and no Facet
// as its own ancestor (Req 9.2).
func (r *FacetRepo) Reparent(ctx context.Context, rc data.RequestContext, facetRecordID string, newParentRecordID *string) error {
	fid, err := recordid.Parse(facetRecordID)
	if err != nil {
		return ErrNotAccessible
	}
	facetSurrogate, err := r.resolveRecord(ctx, rc, RecordFacet, fid)
	if err != nil {
		return err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	// Top-level move: clear the parent. No hierarchy check is needed because a
	// Facet with no parent cannot participate in a cycle and stays in its Sphere.
	if newParentRecordID == nil {
		if _, err := tdb.DB().ExecContext(ctx,
			`UPDATE facet SET parent_facet_id = NULL WHERE id = ?`, facetSurrogate,
		); err != nil {
			return fmt.Errorf("reparent facet to top level: %w", err)
		}
		return nil
	}

	pid, err := recordid.Parse(*newParentRecordID)
	if err != nil {
		return ErrNotAccessible
	}
	parentSurrogate, err := r.resolveRecord(ctx, rc, RecordFacet, pid)
	if err != nil {
		return err
	}

	// Reject a Facet as its own parent (the simplest cycle — Req 9.1). Checked
	// before any write so the hierarchy is untouched on rejection.
	if parentSurrogate == facetSurrogate {
		return fmt.Errorf("%w: a facet cannot be its own parent", ErrHierarchy)
	}

	// The proposed parent must live in the same Sphere as the Facet (Req 9.3).
	var facetSphere, parentSphere int64
	if err := tdb.DB().QueryRowContext(ctx,
		`SELECT sphere_id FROM facet WHERE id = ?`, facetSurrogate,
	).Scan(&facetSphere); err != nil {
		return fmt.Errorf("load facet sphere: %w", err)
	}
	if err := tdb.DB().QueryRowContext(ctx,
		`SELECT sphere_id FROM facet WHERE id = ?`, parentSurrogate,
	).Scan(&parentSphere); err != nil {
		return fmt.Errorf("load parent facet sphere: %w", err)
	}
	if facetSphere != parentSphere {
		return fmt.Errorf("%w: parent facet is in a different sphere", ErrHierarchy)
	}

	// Reject a parent that is a descendant of the Facet (Req 9.1). Walk the
	// proposed parent's ancestor chain upward; if the Facet being moved appears
	// as an ancestor of the proposed parent, the move would create a cycle.
	isDescendant, err := parentHasAncestor(ctx, tdb.DB(), parentSurrogate, facetSurrogate)
	if err != nil {
		return err
	}
	if isDescendant {
		return fmt.Errorf("%w: parent is a descendant of the facet", ErrHierarchy)
	}

	// All checks passed; a single UPDATE performs the move.
	if _, err := tdb.DB().ExecContext(ctx,
		`UPDATE facet SET parent_facet_id = ? WHERE id = ?`, parentSurrogate, facetSurrogate,
	); err != nil {
		return fmt.Errorf("reparent facet: %w", err)
	}
	return nil
}

// parentHasAncestor reports whether ancestorID appears anywhere on the
// parent_facet_id chain at or above startID (inclusive of startID itself). It
// walks upward from startID following parent_facet_id, which is guaranteed to
// terminate because the existing hierarchy is acyclic. It is used to detect
// whether making startID a Facet's parent would place that Facet's own subtree
// above it — i.e. form a cycle.
func parentHasAncestor(ctx context.Context, db *sql.DB, startID, ancestorID int64) (bool, error) {
	current := sql.NullInt64{Int64: startID, Valid: true}
	for current.Valid {
		if current.Int64 == ancestorID {
			return true, nil
		}
		var parent sql.NullInt64
		err := db.QueryRowContext(ctx,
			`SELECT parent_facet_id FROM facet WHERE id = ?`, current.Int64,
		).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			// The chain reached a row that no longer exists; stop walking.
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("walk facet ancestors: %w", err)
		}
		current = parent
	}
	return false, nil
}
