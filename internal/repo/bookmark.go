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

// This file implements the Bookmark repository: a User's personal list of
// Polygons they can return to, grouped by owning Sphere (design.md —
// "Bookmarks menu (Req 26)"; Requirements 26.1–26.5). A Bookmark is a marker a
// User places on a Polygon; it is per-user, per-tenant state that lives in the
// tenant DB's BOOKMARK table even though user_id references a Central_Directory
// user (design.md — data model notes).
//
// Two invariants live here:
//
//   - Add is idempotent. Bookmarking a Polygon adds it to the caller's list
//     (Req 26.1); bookmarking one already present leaves the list unchanged
//     (Req 26.2). The table's PRIMARY KEY(user_id, polygon_id) makes a repeat a
//     no-op, expressed as INSERT OR IGNORE, so a second add neither errors nor
//     duplicates.
//
//   - Grouping is by owning Sphere. Listing returns the caller's bookmarked
//     Polygons partitioned by the Sphere that owns each (Req 26.3); every
//     bookmark appears under exactly its own Sphere. When the caller has no
//     bookmarks the result is empty and Empty reports true, which the UI renders
//     as the empty-bookmarks indicator (Req 26.4).
//
// Like every repository here, BookmarkRepo embeds Base and reaches the database
// only through the caller's own tenant handle, so all reads and writes are
// structurally scoped to rc.TenantID (Req 1.4, 1.5). Bookmarks are also scoped
// to rc.UserID: every statement filters by the authenticated user's id, so one
// user never sees or mutates another's list. A Polygon Record_ID that belongs
// to another tenant is simply absent here and resolves to ErrNotAccessible
// (Req 1.6), so a cross-tenant add/remove changes nothing.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// SphereGroup is one Sphere's worth of a User's bookmarks: the owning Sphere
// and the bookmarked Polygons within it. ListBookmarks returns one SphereGroup
// per Sphere that has at least one of the caller's bookmarks (Req 26.3).
type SphereGroup struct {
	// Sphere is the owning Sphere the Polygons below belong to.
	Sphere Sphere
	// Polygons are the caller's bookmarked Polygons within Sphere. Non-empty
	// for every group returned.
	Polygons []Polygon
}

// BookmarkList is the result of listing a User's bookmarks: the per-Sphere
// groups plus a convenience flag for the empty case. Empty is true exactly when
// Groups is empty, which the UI renders as the empty-bookmarks indicator
// (Req 26.4).
type BookmarkList struct {
	// Groups holds one entry per owning Sphere, each with that Sphere's
	// bookmarked Polygons (Req 26.3).
	Groups []SphereGroup
	// Empty reports that the caller has no bookmarks (Req 26.4).
	Empty bool
}

// BookmarkRepo is the tenant- and user-scoped Bookmark repository. It embeds
// Base so every query is routed to the caller's own tenant DB and nowhere else;
// all statements additionally filter by rc.UserID.
type BookmarkRepo struct {
	Base
}

// NewBookmarkRepo constructs a BookmarkRepo over a ConnManager.
func NewBookmarkRepo(conns data.ConnManager) *BookmarkRepo {
	return &BookmarkRepo{Base: NewBase(conns)}
}

// AddBookmark adds the Polygon identified by polygonRecordID to the caller's
// bookmark list (Req 26.1). It is idempotent: if the Polygon is already
// bookmarked the list is left unchanged (Req 26.2), achieved with INSERT OR
// IGNORE against the (user_id, polygon_id) primary key.
//
// The Polygon is resolved within the caller's tenant first, so a Record_ID that
// lives in another tenant is absent and returns ErrNotAccessible with nothing
// changed (Req 1.6).
func (r *BookmarkRepo) AddBookmark(ctx context.Context, rc data.RequestContext, polygonRecordID string) error {
	pid, err := recordid.Parse(polygonRecordID)
	if err != nil {
		return ErrNotAccessible
	}
	polygonSurrogate, err := r.resolveRecord(ctx, rc, RecordPolygon, pid)
	if err != nil {
		return err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	// INSERT OR IGNORE makes the second add a no-op via the primary key, so an
	// already-bookmarked Polygon leaves the list unchanged (Req 26.2).
	if _, err := tdb.DB().ExecContext(ctx,
		`INSERT OR IGNORE INTO bookmark (user_id, polygon_id) VALUES (?, ?)`,
		int64(rc.UserID()), polygonSurrogate,
	); err != nil {
		return fmt.Errorf("add bookmark: %w", err)
	}
	return nil
}

// RemoveBookmark removes the Polygon identified by polygonRecordID from the
// caller's bookmark list (Req 26.5). Removing a Polygon that is not bookmarked
// is a no-op — the list is already in the desired state — and reports no error.
//
// The Polygon is resolved within the caller's tenant first, so a Record_ID that
// lives in another tenant is absent and returns ErrNotAccessible with nothing
// changed (Req 1.6).
func (r *BookmarkRepo) RemoveBookmark(ctx context.Context, rc data.RequestContext, polygonRecordID string) error {
	pid, err := recordid.Parse(polygonRecordID)
	if err != nil {
		return ErrNotAccessible
	}
	polygonSurrogate, err := r.resolveRecord(ctx, rc, RecordPolygon, pid)
	if err != nil {
		return err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	if _, err := tdb.DB().ExecContext(ctx,
		`DELETE FROM bookmark WHERE user_id = ? AND polygon_id = ?`,
		int64(rc.UserID()), polygonSurrogate,
	); err != nil {
		return fmt.Errorf("remove bookmark: %w", err)
	}
	return nil
}

// ListBookmarks returns the caller's bookmarked Polygons grouped by their
// owning Sphere (Req 26.3). Each returned SphereGroup names one Sphere and
// carries only the caller's bookmarks within it; every bookmark appears under
// exactly its own Sphere. Groups are ordered by Sphere name (case-insensitive,
// Record_ID tie-break) and Polygons within a group by creation time then
// Record_ID for a stable result.
//
// When the caller has no bookmarks the returned BookmarkList has no groups and
// Empty is true, which the UI renders as the empty-bookmarks indicator
// (Req 26.4).
//
// Everything is scoped to the caller's own tenant and user: the query filters
// by rc.UserID and runs only against rc.TenantID's DB, so a user never sees
// another user's or another tenant's bookmarks (Req 1.5, 1.6).
func (r *BookmarkRepo) ListBookmarks(ctx context.Context, rc data.RequestContext) (BookmarkList, error) {
	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return BookmarkList{}, err
	}

	// Join bookmarks -> polygon -> sphere (and optional facet/parent) so each
	// row carries both the owning Sphere and the full Polygon. Ordering by
	// sphere then polygon gives a stable, already-grouped stream we fold into
	// SphereGroups in one pass.
	const q = `
		SELECT s.record_id, s.name, s.created_at,
		       p.record_id, f.record_id, pp.record_id,
		       p.type, p.content, p.edit_mode, p.author_user_id, p.created_at, p.updated_at
		FROM bookmark b
		JOIN polygon p ON p.id = b.polygon_id
		JOIN sphere s ON s.id = p.sphere_id
		LEFT JOIN facet f ON f.id = p.facet_id
		LEFT JOIN polygon pp ON pp.id = p.parent_polygon_id
		WHERE b.user_id = ?
		ORDER BY s.name COLLATE NOCASE ASC, s.record_id ASC, p.created_at, p.record_id`

	rows, err := tdb.DB().QueryContext(ctx, q, int64(rc.UserID()))
	if err != nil {
		return BookmarkList{}, fmt.Errorf("list bookmarks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var groups []SphereGroup
	for rows.Next() {
		var (
			sphereCanonical string
			sphereName      string
			sphereCreatedAt string
			poly            Polygon
			facet           sql.NullString
			parent          sql.NullString
			typ             string
			editMode        string
		)
		if err := rows.Scan(
			&sphereCanonical, &sphereName, &sphereCreatedAt,
			&poly.RecordID, &facet, &parent,
			&typ, &poly.Content, &editMode, &poly.AuthorUserID, &poly.CreatedAt, &poly.UpdatedAt,
		); err != nil {
			return BookmarkList{}, fmt.Errorf("scan bookmark: %w", err)
		}

		sphereID, err := recordid.Parse(sphereCanonical)
		if err != nil {
			return BookmarkList{}, fmt.Errorf("parse sphere record id: %w", err)
		}
		poly.SphereRecordID = sphereCanonical
		poly.Type = PolygonType(typ)
		poly.EditMode = EditMode(editMode)
		if facet.Valid {
			poly.FacetRecordID = &facet.String
		}
		if parent.Valid {
			poly.ParentRecordID = &parent.String
		}

		// Rows arrive already ordered by Sphere, so a new Sphere either starts a
		// fresh group or extends the current tail — every Polygon lands under
		// exactly its owning Sphere (Req 26.3).
		if n := len(groups); n > 0 && groups[n-1].Sphere.RecordID == sphereID {
			groups[n-1].Polygons = append(groups[n-1].Polygons, poly)
			continue
		}
		groups = append(groups, SphereGroup{
			Sphere:   Sphere{RecordID: sphereID, Name: sphereName, CreatedAt: sphereCreatedAt},
			Polygons: []Polygon{poly},
		})
	}
	if err := rows.Err(); err != nil {
		return BookmarkList{}, fmt.Errorf("iterate bookmarks: %w", err)
	}

	if len(groups) == 0 {
		// No bookmarks — the UI shows the empty-bookmarks indicator (Req 26.4).
		return BookmarkList{Empty: true}, nil
	}
	return BookmarkList{Groups: groups}, nil
}
