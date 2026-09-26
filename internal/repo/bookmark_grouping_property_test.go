package repo

import (
	"context"
	"sort"
	"testing"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 29: Bookmarks grouped by Sphere
//
// For all bookmark sets, the bookmarks menu partitions the user's bookmarked
// Polygons by their owning Sphere with every bookmark appearing under exactly
// its own Sphere.
//
// Validates: Requirements 26.3
//
// The property runs over the real production tenant-routing path: file-backed
// tenants behind a real ConnManager (the twoTenantFixture pattern from
// base_test.go, reused via newBookmarkFixture). A single fixture seeds a fixed
// universe of Polygons spread across several Spheres in tenant A, recording for
// each Polygon the Record_ID of its owning Sphere. Each rapid run draws an
// arbitrary subset of that universe to bookmark, then checks that ListBookmarks
// partitions the caller's bookmarks correctly:
//
//   - Correct home (Req 26.3): every bookmarked Polygon appears under exactly
//     its owning Sphere group, and every Polygon reported in a group actually
//     belongs to that group's Sphere.
//
//   - Exact partition (Req 26.3): each returned group holds exactly that
//     Sphere's bookmarked Polygons — no more, no fewer — and only Spheres with
//     at least one bookmark yield a group.
//
//   - Union equals the whole set (Req 26.3): the union of Polygons across all
//     groups equals the full bookmarked set, with no duplicates (each bookmark
//     appears once) and no omissions.
//
// The universe spans several Spheres with varying counts so a drawn subset can
// leave some Spheres empty (they must not appear as groups) and populate others
// partially, exercising the partition boundaries.
func TestBookmarkPropertyGroupedBySphere(t *testing.T) {
	f := newBookmarkFixture(t)
	ctx := context.Background()

	// Seed a fixed universe: four Spheres in tenant A, each with a handful of
	// Polygons. polygonOwner maps each Polygon's canonical Record_ID to the
	// canonical Record_ID of its owning Sphere; polyRIDs is the full universe.
	type sphereSeed struct {
		rid     recordid.ID
		name    string
		polyCnt int
	}
	seeds := []sphereSeed{
		{recordid.New(), "Alpha", 3},
		{recordid.New(), "Bravo", 2},
		{recordid.New(), "Charlie", 4},
		{recordid.New(), "Delta", 1},
	}

	polygonOwner := make(map[string]string) // polygon canonical -> sphere canonical
	var polyRIDs []string
	for _, s := range seeds {
		surrogate := seedSphere(t, f.dbA, s.rid, s.name)
		for i := 0; i < s.polyCnt; i++ {
			_, rid := seedPolygonWithRecordID(t, f.dbA, surrogate)
			polygonOwner[rid.Canonical()] = s.rid.Canonical()
			polyRIDs = append(polyRIDs, rid.Canonical())
		}
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Start each run from a clean bookmark list.
		for _, rid := range polyRIDs {
			if err := f.repo.RemoveBookmark(ctx, f.rcA, rid); err != nil {
				rt.Fatalf("pre-clean remove: %v", err)
			}
		}

		// Draw an arbitrary subset of the universe to bookmark. The model set
		// records which Polygons should end up bookmarked, and modelBySphere
		// records the expected grouping (owning Sphere -> its bookmarked
		// Polygons).
		model := make(map[string]bool)
		modelBySphere := make(map[string]map[string]bool)
		for _, rid := range polyRIDs {
			if rapid.Bool().Draw(rt, "bookmark:"+rid) {
				if err := f.repo.AddBookmark(ctx, f.rcA, rid); err != nil {
					rt.Fatalf("add %s: %v", rid, err)
				}
				model[rid] = true
				owner := polygonOwner[rid]
				if modelBySphere[owner] == nil {
					modelBySphere[owner] = make(map[string]bool)
				}
				modelBySphere[owner][rid] = true
			}
		}

		list, err := f.repo.ListBookmarks(ctx, f.rcA)
		if err != nil {
			rt.Fatalf("list bookmarks: %v", err)
		}

		// Empty flag must agree with whether any bookmark was drawn.
		if list.Empty != (len(model) == 0) {
			rt.Fatalf("Empty = %v, want %v (model has %d bookmarks)",
				list.Empty, len(model) == 0, len(model))
		}

		// Only Spheres with at least one bookmark may appear as groups, and each
		// Sphere appears at most once (partition, not a multiset of groups).
		if len(list.Groups) != len(modelBySphere) {
			rt.Fatalf("group count = %d, want %d", len(list.Groups), len(modelBySphere))
		}
		seenSpheres := make(map[string]bool)

		// union accumulates every Polygon reported across all groups, so we can
		// confirm the union equals the full bookmarked set with no duplicates.
		union := make(map[string]bool)

		for _, g := range list.Groups {
			sphereCanon := g.Sphere.RecordID.Canonical()

			// Each Sphere group appears at most once.
			if seenSpheres[sphereCanon] {
				rt.Fatalf("sphere %s appears in more than one group", sphereCanon)
			}
			seenSpheres[sphereCanon] = true

			// The group's Sphere must be one that actually has bookmarks.
			wantMembers, ok := modelBySphere[sphereCanon]
			if !ok {
				rt.Fatalf("group for sphere %s has no expected bookmarks", sphereCanon)
			}

			// Each group holds exactly that Sphere's bookmarked Polygons, with
			// every Polygon reporting the Sphere it was grouped under (Req 26.3).
			var gotMembers []string
			for _, p := range g.Polygons {
				// Correct home: the Polygon's own SphereRecordID matches the
				// group it landed in, and matches its true owning Sphere.
				if p.SphereRecordID != sphereCanon {
					rt.Fatalf("polygon %s grouped under %s but reports sphere %s",
						p.RecordID, sphereCanon, p.SphereRecordID)
				}
				if owner := polygonOwner[p.RecordID]; owner != sphereCanon {
					rt.Fatalf("polygon %s owned by %s but grouped under %s",
						p.RecordID, owner, sphereCanon)
				}
				// No duplicates across the whole result.
				if union[p.RecordID] {
					rt.Fatalf("polygon %s appears in more than one group", p.RecordID)
				}
				union[p.RecordID] = true
				gotMembers = append(gotMembers, p.RecordID)
			}

			// Exact per-group membership: the group's Polygons equal exactly the
			// expected bookmarked Polygons for that Sphere.
			want := make([]string, 0, len(wantMembers))
			for rid := range wantMembers {
				want = append(want, rid)
			}
			sort.Strings(want)
			sort.Strings(gotMembers)
			if !equalStrings(gotMembers, want) {
				rt.Fatalf("sphere %s members = %v, want %v", sphereCanon, gotMembers, want)
			}
		}

		// Union across groups equals the full bookmarked set: no omissions.
		gotUnion := make([]string, 0, len(union))
		for rid := range union {
			gotUnion = append(gotUnion, rid)
		}
		wantUnion := make([]string, 0, len(model))
		for rid := range model {
			wantUnion = append(wantUnion, rid)
		}
		sort.Strings(gotUnion)
		sort.Strings(wantUnion)
		if !equalStrings(gotUnion, wantUnion) {
			rt.Fatalf("union across groups = %v, want full bookmark set %v", gotUnion, wantUnion)
		}
	})
}
