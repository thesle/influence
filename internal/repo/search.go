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

// This file implements content search query scoping (design.md — "Search —
// Req 23, 24"; Requirements 23.1, 23.3). It is the read side of the search
// feature: the write side (keeping the FTS5 index in sync) lives in
// polygon_index.go, and this component answers a query against that index.
//
// The single invariant this component protects is access scoping: a search
// returns ONLY Polygons that both (a) match the query text AND (b) reside in a
// Sphere the caller can read (Req 23.1). Two seams enforce it:
//
//   - The FTS5 MATCH against polygon_fts supplies the "matches the query" half.
//     Because the index holds only MASKED content (polygon_index.go), sensitive
//     plaintext is not in the index and can never match, so the non-leakage
//     guarantee (Req 16.7, 23.2) is inherited here for free.
//
//   - Each candidate hit is joined back to its Polygon to recover the owning
//     Sphere's surrogate id, and that Sphere is checked against the caller's
//     effective Sphere grants carried on the RequestContext (built by the
//     middleware from the PolicyEngine — chain.go). The grant map already has
//     admin implicit grants and the deactivated override applied, and omits any
//     Sphere the caller cannot reach, so a Polygon in an inaccessible Sphere is
//     dropped even when its masked content matches the query.
//
// When nothing matches — no FTS hit, or every hit lands in a Sphere the caller
// cannot read — the result is an empty (non-nil-semantics) slice (Req 23.3).
//
// Like every repository here, SearchRepo embeds Base and reaches the database
// only through the caller's own tenant handle, so the whole query is
// structurally scoped to rc.TenantID (Req 1.4, 1.5).

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/influence/influence/internal/data"
)

// SearchRepo runs content search queries over the tenant-scoped polygon_fts
// index and scopes results to the Spheres the caller can access. It embeds Base
// so every statement is routed to the caller's own tenant DB and nowhere else.
type SearchRepo struct {
	Base
}

// NewSearchRepo constructs a SearchRepo over a ConnManager.
func NewSearchRepo(conns data.ConnManager) *SearchRepo {
	return &SearchRepo{Base: NewBase(conns)}
}

// Search returns the Polygons whose MASKED content matches query and that reside
// in a Sphere the caller can read (Req 23.1). Results are ordered by FTS5
// relevance (rank) so the closest matches come first, with the Polygon
// Record_ID as a stable tiebreaker.
//
// Scoping is enforced in two layers. The FTS5 MATCH selects candidate hits from
// the tenant's polygon_fts index; each hit is joined back to its Polygon to
// recover the owning Sphere's surrogate id, and a Polygon is included ONLY when
// the caller holds a grant of AccessRead or better on that Sphere. The grant
// map on the RequestContext is the effective map the middleware computed through
// the PolicyEngine (admin implicit grants and the deactivated override already
// applied, inaccessible Spheres omitted), so a match in a Sphere the caller
// cannot reach is silently excluded rather than surfaced.
//
// An empty or whitespace-only query, or a query that matches no readable
// Polygon, yields an empty result set with no error (Req 23.3). The whole
// operation is scoped to the caller's own tenant via Base (Req 1.4, 1.5) and is
// read-only, so it changes no data.
func (r *SearchRepo) Search(ctx context.Context, rc data.RequestContext, query string) ([]Polygon, error) {
	// A blank query has no search terms and therefore matches nothing; short
	// circuit to an empty set rather than handing FTS5 an empty MATCH string
	// (which is a query syntax error) (Req 23.3).
	if isBlank(query) {
		return []Polygon{}, nil
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return nil, err
	}

	// Join every FTS hit back to its Polygon to recover the owning Sphere and
	// the placement Record_IDs, exactly as PolygonRepo.Get/List do, so callers
	// get fully-formed Polygons. p.sphere_id is the surrogate the caller's grant
	// map is keyed by, selected so results can be access-filtered below.
	const q = `
		SELECT p.record_id, s.record_id, f.record_id, pp.record_id,
		       p.type, p.content, p.edit_mode, p.author_user_id, p.created_at, p.updated_at,
		       p.sphere_id
		FROM polygon_fts x
		JOIN polygon p ON p.record_id = x.record_id
		JOIN sphere s ON s.id = p.sphere_id
		LEFT JOIN facet f ON f.id = p.facet_id
		LEFT JOIN polygon pp ON pp.id = p.parent_polygon_id
		WHERE polygon_fts MATCH ?
		ORDER BY rank, p.record_id`

	rows, err := tdb.DB().QueryContext(ctx, q, query)
	if err != nil {
		return nil, fmt.Errorf("search polygons: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Start non-nil so a query that matches nothing readable returns an empty
	// (not nil) set, matching the "empty result set" contract (Req 23.3).
	out := []Polygon{}
	for rows.Next() {
		var (
			poly     Polygon
			facet    sql.NullString
			parent   sql.NullString
			typ      string
			editMode string
			sphereID int64
		)
		if err := rows.Scan(
			&poly.RecordID, &poly.SphereRecordID, &facet, &parent,
			&typ, &poly.Content, &editMode, &poly.AuthorUserID, &poly.CreatedAt, &poly.UpdatedAt,
			&sphereID,
		); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}

		// Access scoping (Req 23.1): include the Polygon only when the caller
		// can read its Sphere. The effective grant map omits Spheres the caller
		// cannot reach and collapses AccessNone away, so an absent grant — or a
		// grant below AccessRead — drops the hit even though its masked content
		// matched the query.
		grant, ok := rc.SphereGrant(data.SphereID(sphereID))
		if !ok || grant.Access < data.AccessRead {
			continue
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
		return nil, fmt.Errorf("iterate search hits: %w", err)
	}
	return out, nil
}

// isBlank reports whether s contains only whitespace (or is empty). Such a
// query carries no search terms, so search returns an empty set rather than
// issuing an empty FTS5 MATCH (Req 23.3).
func isBlank(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			continue
		default:
			return false
		}
	}
	return true
}
