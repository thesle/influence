package repo

import (
	"context"
	"sort"
	"testing"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 28: Bookmark idempotence and add/remove round-trip
//
// For all bookmark lists and any Polygon, bookmarking a Polygon already
// bookmarked leaves the list unchanged, and adding then removing a bookmark
// returns the list to its prior state.
//
// Validates: Requirements 26.1, 26.2, 26.5
//
// The property is exercised over the real production tenant-routing path: a
// file-backed tenant behind a real ConnManager (the twoTenantFixture pattern
// from base_test.go). A single fixture seeds a fixed set of Polygons across a
// couple of Spheres once; each rapid run drives an arbitrary sequence of
// add/remove operations against the caller's per-user bookmark list and checks:
//
//   - Set equality (Req 26.1, 26.5): a model set tracks which seeded Polygons
//     are currently added-and-not-removed. After every operation the persisted
//     bookmark set (the Record_IDs listed by ListBookmarks, flattened across
//     Sphere groups) equals that model set exactly.
//
//   - Idempotence (Req 26.2): whenever an add targets a Polygon already in the
//     set, the bookmark set is unchanged by that add.
//
//   - Add/remove round-trip (Req 26.5): adding a not-yet-bookmarked Polygon and
//     then removing it returns the bookmark set to exactly its prior state.
//
// The seeded Polygons span two Spheres so the flattened set spans groups, and
// operations target a Polygon drawn from the fixed set so re-adds and
// remove-absent cases occur naturally across a run.
func TestBookmarkPropertyIdempotenceAndRoundTrip(t *testing.T) {
	f := newBookmarkFixture(t)
	ctx := context.Background()

	// Seed a fixed set of Polygons across two Spheres in tenant A.
	sphereA := seedSphere(t, f.dbA, recordid.New(), "Alpha")
	sphereB := seedSphere(t, f.dbA, recordid.New(), "Bravo")
	var polyRIDs []recordid.ID
	for i := 0; i < 3; i++ {
		_, rid := seedPolygonWithRecordID(t, f.dbA, sphereA)
		polyRIDs = append(polyRIDs, rid)
	}
	for i := 0; i < 2; i++ {
		_, rid := seedPolygonWithRecordID(t, f.dbA, sphereB)
		polyRIDs = append(polyRIDs, rid)
	}

	// bookmarkedSet returns the set of currently-bookmarked Polygon Record_IDs
	// (canonical form), flattened across the Sphere groups ListBookmarks returns
	// and sorted for a stable comparison.
	bookmarkedSet := func(rt *rapid.T) []string {
		list, err := f.repo.ListBookmarks(ctx, f.rcA)
		if err != nil {
			rt.Fatalf("list bookmarks: %v", err)
		}
		var out []string
		for _, g := range list.Groups {
			for _, p := range g.Polygons {
				out = append(out, p.RecordID)
			}
		}
		sort.Strings(out)
		return out
	}

	// setEquals reports whether the persisted bookmark set equals the model set.
	setEquals := func(rt *rapid.T, model map[string]bool) []string {
		want := make([]string, 0, len(model))
		for rid := range model {
			want = append(want, rid)
		}
		sort.Strings(want)
		got := bookmarkedSet(rt)
		if !equalStrings(got, want) {
			rt.Fatalf("bookmark set mismatch: got %v, want %v", got, want)
		}
		return got
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Start each run from a clean list so the model and the persisted set
		// begin in agreement (empty).
		for _, rid := range polyRIDs {
			if err := f.repo.RemoveBookmark(ctx, f.rcA, rid.Canonical()); err != nil {
				rt.Fatalf("pre-clean remove: %v", err)
			}
		}
		model := make(map[string]bool)
		setEquals(rt, model)

		steps := rapid.IntRange(1, 12).Draw(rt, "steps")
		for s := 0; s < steps; s++ {
			idx := rapid.IntRange(0, len(polyRIDs)-1).Draw(rt, "target")
			target := polyRIDs[idx].Canonical()
			op := rapid.SampledFrom([]string{"add", "remove", "roundtrip"}).Draw(rt, "op")

			switch op {
			case "add":
				alreadyIn := model[target]
				before := setEquals(rt, model)
				if err := f.repo.AddBookmark(ctx, f.rcA, target); err != nil {
					rt.Fatalf("add %s: %v", target, err)
				}
				model[target] = true
				after := setEquals(rt, model)
				// Idempotence (Req 26.2): re-adding an already-bookmarked
				// Polygon leaves the set unchanged.
				if alreadyIn && !equalStrings(before, after) {
					rt.Fatalf("idempotence violated: re-add changed set: before=%v after=%v",
						before, after)
				}

			case "remove":
				if err := f.repo.RemoveBookmark(ctx, f.rcA, target); err != nil {
					rt.Fatalf("remove %s: %v", target, err)
				}
				delete(model, target)
				setEquals(rt, model)

			case "roundtrip":
				// Add-then-remove round-trip (Req 26.5): capture the prior set,
				// add the target and remove it, and confirm the set is restored.
				before := setEquals(rt, model)
				if err := f.repo.AddBookmark(ctx, f.rcA, target); err != nil {
					rt.Fatalf("roundtrip add %s: %v", target, err)
				}
				if err := f.repo.RemoveBookmark(ctx, f.rcA, target); err != nil {
					rt.Fatalf("roundtrip remove %s: %v", target, err)
				}
				// The model is unchanged only when the target was not already
				// bookmarked; when it was, the round-trip removes it, so mirror
				// that in the model before comparing.
				wasIn := model[target]
				delete(model, target)
				after := setEquals(rt, model)
				if !wasIn && !equalStrings(before, after) {
					rt.Fatalf("round-trip violated: add-then-remove changed set: before=%v after=%v",
						before, after)
				}
			}
		}
	})
}
