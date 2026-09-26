package repo

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 26: "Recently Viewed Polygons" cap and order
//
// For all per-user view histories, the "Recently Viewed Polygons" result is the
// up-to-50 most recently viewed distinct Polygons in accessible Spheres, ordered
// from most recent to least recent.
//
// Validates: Requirements 24.2
//
// The property runs over the real production tenant-routing path: file-backed
// tenants behind a real ConnManager (the twoTenantFixture, via the
// quickSearchFixture from quicksearch_test.go). It reuses that file's fixtures —
// quickSearchFixture / newQuickSearchFixture, rcWithAccess, seedSphere,
// seedPolygonAuthored, seedView — rather than redefining them.
//
// Each rapid run:
//
//  1. draws a small set of Spheres, each independently marked accessible or
//     not, and seeds them into tenant A;
//  2. draws a set of Polygons, each placed in one of those Spheres and seeded
//     into tenant A;
//  3. draws a set of VIEW_LOG entries: each entry views one of the Polygons at
//     a distinct view time (a Polygon may be viewed many times, across
//     accessible and inaccessible Spheres, at various times), and seeds them;
//  4. calls RecentlyViewed for a caller granted read on exactly the accessible
//     Spheres and asserts the returned list equals the independently computed
//     ground truth — distinct Polygons in accessible Spheres, each at its
//     latest view time, ordered most-recent-view first, capped at 50.
//
// View times are drawn to be pairwise-distinct integers rendered as ISO 8601
// UTC. Distinct times make the "most recent" ordering total and unambiguous, so
// the property can assert the exact ordered list rather than a weaker relation,
// while still exercising the distinct-collapse, accessibility filter, and the
// 50-entry cap (the generators can produce well over 50 distinct viewed
// Polygons).
func TestRecentlyViewedPropertyCapAndOrder(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10

	// A base instant; distinct integer offsets (in minutes) become distinct
	// ISO 8601 UTC view times with a total order matching the integer order.
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	viewTime := func(offset int) string {
		return base.Add(time.Duration(offset) * time.Minute).Format(time.RFC3339)
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Fresh tenant state per run so each run's ground truth is computed
		// against exactly its own seeded set.
		if _, err := f.dbA.Exec(`DELETE FROM view_log`); err != nil {
			rt.Fatalf("reset view_log: %v", err)
		}
		if _, err := f.dbA.Exec(`DELETE FROM polygon`); err != nil {
			rt.Fatalf("reset polygon: %v", err)
		}
		if _, err := f.dbA.Exec(`DELETE FROM sphere`); err != nil {
			rt.Fatalf("reset sphere: %v", err)
		}

		// Draw a set of Spheres, each independently accessible or not.
		sphereCount := rapid.IntRange(1, 4).Draw(rt, "sphereCount")
		type sphereInfo struct {
			surrogate  int64
			recordID   string
			accessible bool
		}
		spheres := make([]sphereInfo, sphereCount)
		for i := range spheres {
			rid := recordid.New()
			surrogate := seedSphere(t, f.dbA, rid, fmt.Sprintf("Sphere %d", i))
			spheres[i] = sphereInfo{
				surrogate:  surrogate,
				recordID:   rid.Canonical(),
				accessible: rapid.Bool().Draw(rt, fmt.Sprintf("accessible_%d", i)),
			}
		}

		// Draw a set of Polygons, each placed in a drawn Sphere. Enough Polygons
		// that the 50-cap can bind. Every Polygon is authored by me so the seed
		// helper is happy; authorship is irrelevant to RecentlyViewed.
		polygonCount := rapid.IntRange(0, 70).Draw(rt, "polygonCount")
		type polygonInfo struct {
			surrogate   int64
			recordID    string
			sphereIndex int
		}
		polygons := make([]polygonInfo, polygonCount)
		for i := range polygons {
			sIdx := rapid.IntRange(0, sphereCount-1).Draw(rt, fmt.Sprintf("polySphere_%d", i))
			rid, sur := seedPolygonAuthored(t, f.dbA, spheres[sIdx].surrogate, me, base.Format(time.RFC3339))
			polygons[i] = polygonInfo{
				surrogate:   sur,
				recordID:    rid.Canonical(),
				sphereIndex: sIdx,
			}
		}

		// Draw a set of view events. Each event views one Polygon at a distinct
		// view time; a Polygon may be viewed multiple times. Skip entirely when
		// there are no Polygons to view.
		latestView := map[int]int{} // polygon index -> latest view offset seen
		if polygonCount > 0 {
			viewCount := rapid.IntRange(0, 200).Draw(rt, "viewCount")
			// Distinct, strictly-increasing offsets guarantee pairwise-distinct
			// view times and a total order; the draw order does not need to match
			// time order because the offset carries the ordering.
			offset := 1
			for v := 0; v < viewCount; v++ {
				pIdx := rapid.IntRange(0, polygonCount-1).Draw(rt, fmt.Sprintf("view_%d_poly", v))
				// Advance the offset by a drawn positive step so times are distinct
				// and the sequence is strictly increasing.
				offset += rapid.IntRange(1, 5).Draw(rt, fmt.Sprintf("view_%d_step", v))
				seedView(t, f.dbA, me, polygons[pIdx].surrogate, viewTime(offset))
				// Record the latest (largest) offset for this Polygon.
				if cur, ok := latestView[pIdx]; !ok || offset > cur {
					latestView[pIdx] = offset
				}
			}
		}

		// Build the caller's grants: read on exactly the accessible Spheres.
		var accessibleSurrogates []int64
		for _, s := range spheres {
			if s.accessible {
				accessibleSurrogates = append(accessibleSurrogates, s.surrogate)
			}
		}
		rc := rcWithAccess(me, 1, accessibleSurrogates...)

		// Independently compute ground truth: for each distinct Polygon that was
		// viewed AND resides in an accessible Sphere, its latest view offset.
		var candidates []recentEntry
		for pIdx, off := range latestView {
			p := polygons[pIdx]
			if !spheres[p.sphereIndex].accessible {
				continue
			}
			candidates = append(candidates, recentEntry{
				recordID:       p.recordID,
				sphereRecordID: spheres[p.sphereIndex].recordID,
				latestOffset:   off,
			})
		}
		// Order most-recent-view first. Ties are impossible: distinct offsets, and
		// each Polygon appears once. Match the implementation's stable secondary
		// key (record_id DESC) purely for determinism, though it never engages.
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].latestOffset != candidates[j].latestOffset {
				return candidates[i].latestOffset > candidates[j].latestOffset
			}
			return candidates[i].recordID > candidates[j].recordID
		})
		// Cap at 50.
		if len(candidates) > recentlyViewedCap {
			candidates = candidates[:recentlyViewedCap]
		}

		got, err := f.qs.RecentlyViewed(ctx, rc)
		if err != nil {
			rt.Fatalf("recently viewed: %v", err)
		}

		// Length: exactly the capped distinct-accessible-viewed count.
		if len(got) != len(candidates) {
			rt.Fatalf("result len = %d, want %d\n want ids = %v\n got ids  = %v",
				len(got), len(candidates), entryIDs(candidates), resultIDs(got))
		}

		// The cap is never exceeded.
		if len(got) > recentlyViewedCap {
			rt.Fatalf("result len = %d exceeds cap %d", len(got), recentlyViewedCap)
		}

		// Distinct: no Polygon appears twice.
		seen := map[string]bool{}
		for _, r := range got {
			if seen[r.PolygonRecordID] {
				rt.Fatalf("duplicate Polygon %s in result %v", r.PolygonRecordID, resultIDs(got))
			}
			seen[r.PolygonRecordID] = true
		}

		// Ordered, distinct, accessible, and each at its latest view time — the
		// positional comparison against ground truth proves all four at once.
		for i, want := range candidates {
			if got[i].PolygonRecordID != want.recordID {
				rt.Fatalf("position %d = %s, want %s\n want ids = %v\n got ids  = %v",
					i, got[i].PolygonRecordID, want.recordID, entryIDs(candidates), resultIDs(got))
			}
			if got[i].SphereRecordID != want.sphereRecordID {
				rt.Fatalf("position %d sphere = %s, want %s", i, got[i].SphereRecordID, want.sphereRecordID)
			}
			// The ordering timestamp must be the Polygon's most recent view time.
			if got[i].When != viewTime(want.latestOffset) {
				rt.Fatalf("position %d (%s) When = %q, want latest view %q",
					i, want.recordID, got[i].When, viewTime(want.latestOffset))
			}
		}
	})
}

// recentEntry is one expected "Recently Viewed" row in the property's
// independently computed ground truth: a distinct viewed Polygon in an
// accessible Sphere, tagged with its latest view offset for ordering.
type recentEntry struct {
	recordID       string
	sphereRecordID string
	latestOffset   int
}

// entryIDs and resultIDs render Record_ID sequences for readable failure output.
func entryIDs(es []recentEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.recordID)
	}
	return out
}

func resultIDs(rs []QuickSearchResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.PolygonRecordID)
	}
	return out
}
