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

// This file implements the quick-search half of the Search service (design.md —
// "Search — Req 23, 24", "Quick searches (Req 24)"). Quick searches are the
// quick-access lists surfaced in the top bar: the Polygons a User authored and
// the Polygons a User recently viewed. They complement the FTS5 content search
// (task 16.2) but share none of its symbols — this component owns its own file
// and type so the two can be developed in parallel without collision.
//
// Two lists and one supporting write live here (Requirement 24):
//
//   - PolygonsICreated: Polygons authored by the current User, restricted to the
//     Spheres the User can access, newest-created first (Req 24.1).
//   - RecentlyViewed: the User's recently viewed Polygons, restricted to the
//     Spheres the User can access, one entry per Polygon (its most recent view),
//     newest first, capped at the 50 most recent (Req 24.2).
//   - RecordView: append a VIEW_LOG entry so RecentlyViewed has data. This is
//     the write that feeds the recently-viewed list.
//
// Both reads return an empty result set — never an error — when nothing matches:
// a User who has authored nothing, viewed nothing, or whose matches all fall in
// Spheres they cannot access gets back an empty slice (Req 24.3).
//
// Accessibility is per-Sphere. A quick search must only surface Polygons in
// Spheres the caller may access, exactly like the scoped content search
// (Req 23.1). The caller's effective per-Sphere access travels on the
// RequestContext as its Sphere grant map (data.RequestContext.SphereGrant),
// which the middleware/PolicyEngine populated for the request. This component
// reduces that map to the set of accessible Sphere surrogate ids and filters
// every candidate Polygon against it, so a Polygon in a Sphere with no grant
// (AccessNone) is never returned even if the User authored or viewed it.
//
// Like every repository here, QuickSearch embeds Base and reaches the database
// only through the caller's own tenant handle, so all reads and writes are
// structurally scoped to rc.TenantID (Req 1.4, 1.5).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// recentlyViewedCap is the maximum number of entries "Recently Viewed Polygons"
// returns: the 50 most recently viewed distinct Polygons (Req 24.2).
const recentlyViewedCap = 50

// QuickSearchResult is one entry in a quick-search list. It carries the external
// Polygon Record_ID (never the internal surrogate id) and the owning Sphere's
// Record_ID so a caller can render and link the row without a second read. When
// is the ordering timestamp for the list: the Polygon's created_at for
// PolygonsICreated (Req 24.1) and the most recent viewed_at for RecentlyViewed
// (Req 24.2).
type QuickSearchResult struct {
	PolygonRecordID string
	SphereRecordID  string
	When            string
}

// QuickSearch serves the Req 24 quick-access lists. It embeds Base so every
// query is routed to the caller's own tenant DB and nowhere else, and holds no
// state of its own.
type QuickSearch struct {
	Base
}

// NewQuickSearch constructs a QuickSearch over a ConnManager.
func NewQuickSearch(conns data.ConnManager) *QuickSearch {
	return &QuickSearch{Base: NewBase(conns)}
}

// accessibleSphereIDs reduces the caller's per-Sphere grant map to the set of
// Sphere surrogate ids the caller may read. A Sphere is accessible when its
// grant confers at least read access (AccessRead or higher); an absent grant or
// AccessNone is not accessible. The returned set is keyed by the surrogate
// sphere.id — the same integer the tenant's grant map and the polygon.sphere_id
// column use — so it can filter candidate Polygons directly.
//
// The set is used to build the SQL sphere-id filter; it is derived from the
// RequestContext the middleware authenticated the request with, never from
// client input.
func accessibleSphereIDs(rc data.RequestContext) map[data.SphereID]struct{} {
	grants := rc.SphereGrants()
	accessible := make(map[data.SphereID]struct{}, len(grants))
	for id, g := range grants {
		if g.Access >= data.AccessRead {
			accessible[id] = struct{}{}
		}
	}
	return accessible
}

// sphereIDPlaceholders builds a "(?, ?, ...)" placeholder list and the matching
// argument slice for the given accessible Sphere id set, for use in a
// `sphere_id IN (...)` clause. The ids are trusted internal integers, but they
// are still bound as parameters rather than interpolated. The order of args is
// unspecified (set iteration) which is fine for an IN clause.
func sphereIDPlaceholders(accessible map[data.SphereID]struct{}) (string, []any) {
	if len(accessible) == 0 {
		return "", nil
	}
	placeholders := make([]string, 0, len(accessible))
	args := make([]any, 0, len(accessible))
	for id := range accessible {
		placeholders = append(placeholders, "?")
		args = append(args, int64(id))
	}
	return strings.Join(placeholders, ", "), args
}

// PolygonsICreated returns the Polygons authored by the caller (the current
// User on rc), restricted to the Spheres the caller can access, ordered newest-
// created first (created_at DESC), with the Polygon Record_ID as a stable
// tie-break (Req 24.1).
//
// Authorship is matched on POLYGON.author_user_id against the caller's own user
// id, so a caller only ever sees Polygons they created. Accessibility is applied
// on top: a Polygon the caller authored but which lives in a Sphere they can no
// longer access is excluded (Req 23.1). When the caller has authored nothing
// accessible the result is an empty slice, not an error (Req 24.3).
//
// The query runs against the caller's own tenant DB via Base (Req 1.4, 1.5).
func (q *QuickSearch) PolygonsICreated(ctx context.Context, rc data.RequestContext) ([]QuickSearchResult, error) {
	accessible := accessibleSphereIDs(rc)
	if len(accessible) == 0 {
		// No accessible Sphere means nothing can match — empty set (Req 24.3).
		return []QuickSearchResult{}, nil
	}

	tdb, err := q.tenant(ctx, rc)
	if err != nil {
		return nil, err
	}

	inClause, sphereArgs := sphereIDPlaceholders(accessible)
	// author first, then the sphere-id filter args.
	args := make([]any, 0, len(sphereArgs)+1)
	args = append(args, int64(rc.UserID()))
	args = append(args, sphereArgs...)

	query := fmt.Sprintf(`
		SELECT p.record_id, s.record_id, p.created_at
		FROM polygon p
		JOIN sphere s ON s.id = p.sphere_id
		WHERE p.author_user_id = ?
		  AND p.sphere_id IN (%s)
		ORDER BY p.created_at DESC, p.record_id DESC`, inClause)

	rows, err := tdb.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("polygons i created: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []QuickSearchResult{}
	for rows.Next() {
		var r QuickSearchResult
		if err := rows.Scan(&r.PolygonRecordID, &r.SphereRecordID, &r.When); err != nil {
			return nil, fmt.Errorf("scan created polygon: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate created polygons: %w", err)
	}
	return out, nil
}

// RecentlyViewed returns the caller's recently viewed Polygons, restricted to
// the Spheres the caller can access, one entry per Polygon (its most recent
// view), newest first, capped at the 50 most recently viewed (Req 24.2).
//
// The list is built from the caller's own VIEW_LOG entries (VIEW_LOG.user_id =
// the caller's user id). A Polygon viewed many times appears once, ordered by
// its most recent viewed_at (MAX(viewed_at)); ties break on the Polygon
// Record_ID for a stable order. Accessibility is applied so a Polygon the caller
// viewed but can no longer access is excluded (Req 23.1). The result is capped
// at recentlyViewedCap and is an empty slice — never an error — when the caller
// has viewed nothing accessible (Req 24.3).
//
// The query runs against the caller's own tenant DB via Base (Req 1.4, 1.5).
func (q *QuickSearch) RecentlyViewed(ctx context.Context, rc data.RequestContext) ([]QuickSearchResult, error) {
	accessible := accessibleSphereIDs(rc)
	if len(accessible) == 0 {
		return []QuickSearchResult{}, nil
	}

	tdb, err := q.tenant(ctx, rc)
	if err != nil {
		return nil, err
	}

	inClause, sphereArgs := sphereIDPlaceholders(accessible)
	// user id, then the sphere-id filter args, then the cap.
	args := make([]any, 0, len(sphereArgs)+2)
	args = append(args, int64(rc.UserID()))
	args = append(args, sphereArgs...)
	args = append(args, recentlyViewedCap)

	// Collapse a Polygon's many views to its most recent (MAX viewed_at) via
	// GROUP BY, restrict to accessible Spheres, order newest-first, and cap.
	query := fmt.Sprintf(`
		SELECT p.record_id, s.record_id, MAX(v.viewed_at) AS last_viewed
		FROM view_log v
		JOIN polygon p ON p.id = v.polygon_id
		JOIN sphere s ON s.id = p.sphere_id
		WHERE v.user_id = ?
		  AND p.sphere_id IN (%s)
		GROUP BY p.id, p.record_id, s.record_id
		ORDER BY last_viewed DESC, p.record_id DESC
		LIMIT ?`, inClause)

	rows, err := tdb.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("recently viewed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []QuickSearchResult{}
	for rows.Next() {
		var r QuickSearchResult
		if err := rows.Scan(&r.PolygonRecordID, &r.SphereRecordID, &r.When); err != nil {
			return nil, fmt.Errorf("scan recently viewed: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recently viewed: %w", err)
	}
	return out, nil
}

// RecordView appends a VIEW_LOG entry recording that the caller viewed the
// Polygon identified by polygonRecordID at the current time, so the Polygon
// surfaces in RecentlyViewed (Req 24.2). Each view is a new row stamped with the
// view time (ISO 8601 UTC); RecentlyViewed collapses a Polygon's repeated views
// to its most recent, so recording the same Polygon again simply moves it to the
// front of the list.
//
// The Polygon is resolved within the caller's tenant first, so a Record_ID that
// belongs to another tenant is absent and returns ErrNotAccessible with nothing
// written (Req 1.6). The write runs against the caller's own tenant DB via Base
// (Req 1.4, 1.5).
func (q *QuickSearch) RecordView(ctx context.Context, rc data.RequestContext, polygonRecordID string) error {
	pid, err := recordid.Parse(polygonRecordID)
	if err != nil {
		return ErrNotAccessible
	}
	polygonSurrogate, err := q.resolveRecord(ctx, rc, RecordPolygon, pid)
	if err != nil {
		return err
	}

	tdb, err := q.tenant(ctx, rc)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tdb.DB().ExecContext(ctx,
		`INSERT INTO view_log (user_id, polygon_id, viewed_at) VALUES (?, ?, ?)`,
		int64(rc.UserID()), polygonSurrogate, now,
	); err != nil {
		return fmt.Errorf("record view: %w", err)
	}
	return nil
}
