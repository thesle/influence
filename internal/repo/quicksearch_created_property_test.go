package repo

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 25: "Polygons I've Created" filter and order
//
// For all Polygon sets, the "Polygons I've Created" result is exactly the
// Polygons authored by the requesting User that reside in accessible Spheres,
// ordered from most recently created to least recently created.
//
// Validates: Requirements 24.1
//
// The property is exercised over the real production tenant-routing path (the
// twoTenantFixture behind a real ConnManager, reused via the quicksearch
// fixtures from quicksearch_test.go). A single fixture is shared across all
// rapid runs; each run:
//
//   1. draws a fixed pool of Spheres, each independently marked accessible or
//      not, and seeds them into tenant A;
//   2. draws an arbitrary set of Polygons, each with an arbitrary author (the
//      caller or another User), an arbitrary owning Sphere, and an arbitrary
//      created_at time (allowing ties), and seeds them;
//   3. calls PolygonsICreated with a RequestContext granting read access to
//      exactly the accessible Spheres, and asserts the returned Record_IDs
//      equal the independently computed expectation — the caller-authored
//      Polygons in accessible Spheres, ordered created_at DESC with the
//      canonical Record_ID as the stable tie-break (matching the query's
//      `ORDER BY created_at DESC, record_id DESC`).
//
// created_at values are drawn from a small pool of distinct instants so ties
// are common, forcing the Record_ID tie-break to decide order. The tie-break in
// both the query and the Go expectation is a byte-wise comparison of the
// canonical UUID string, identical in Go and SQLite.
func TestPolygonsICreatedPropertyFilterAndOrder(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me int64 = 10
	const other int64 = 99

	// A small pool of distinct created_at instants. Reusing them across many
	// Polygons makes created_at ties common, so the Record_ID tie-break is
	// genuinely exercised.
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	instants := make([]string, 5)
	for i := range instants {
		instants[i] = base.AddDate(0, i, 0).Format(time.RFC3339)
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Fresh tenant state per run so each run's expectation is computed
		// against exactly its own seeded set.
		if _, err := f.dbA.Exec(`DELETE FROM polygon`); err != nil {
			rt.Fatalf("reset polygon: %v", err)
		}
		if _, err := f.dbA.Exec(`DELETE FROM sphere`); err != nil {
			rt.Fatalf("reset sphere: %v", err)
		}

		// Draw a pool of Spheres, each independently accessible or not.
		sphereCount := rapid.IntRange(1, 4).Draw(rt, "sphereCount")
		type sphereInfo struct {
			surrogate  int64
			rid        recordid.ID
			accessible bool
		}
		spheres := make([]sphereInfo, sphereCount)
		var accessibleSurrogates []int64
		for i := range spheres {
			rid := recordid.New()
			sur := seedSphere(t, f.dbA, rid, "Sphere")
			acc := rapid.Bool().Draw(rt, "accessible")
			spheres[i] = sphereInfo{surrogate: sur, rid: rid, accessible: acc}
			if acc {
				accessibleSurrogates = append(accessibleSurrogates, sur)
			}
		}

		// Draw an arbitrary set of Polygons: each has an author (caller or
		// other), an owning Sphere from the pool, and a created_at instant.
		polygonCount := rapid.IntRange(0, 12).Draw(rt, "polygonCount")
		type expected struct {
			rid       string
			createdAt string
		}
		var want []expected
		for i := 0; i < polygonCount; i++ {
			authoredByMe := rapid.Bool().Draw(rt, "authoredByMe")
			author := other
			if authoredByMe {
				author = me
			}
			s := spheres[rapid.IntRange(0, len(spheres)-1).Draw(rt, "sphereIdx")]
			createdAt := instants[rapid.IntRange(0, len(instants)-1).Draw(rt, "instant")]

			rid, _ := seedPolygonAuthored(t, f.dbA, s.surrogate, author, createdAt)

			// A Polygon is expected iff the caller authored it AND it sits in an
			// accessible Sphere.
			if authoredByMe && s.accessible {
				want = append(want, expected{rid: rid.Canonical(), createdAt: createdAt})
			}
		}

		// Independently compute the expected order: created_at DESC, then
		// canonical Record_ID DESC as the stable tie-break.
		sort.SliceStable(want, func(i, j int) bool {
			if want[i].createdAt != want[j].createdAt {
				return want[i].createdAt > want[j].createdAt
			}
			return want[i].rid > want[j].rid
		})
		wantIDs := make([]string, len(want))
		for i, e := range want {
			wantIDs[i] = e.rid
		}

		rc := rcWithAccess(data.UserID(me), 1, accessibleSurrogates...)
		got, err := f.qs.PolygonsICreated(ctx, rc)
		if err != nil {
			rt.Fatalf("polygons i created: %v", err)
		}
		gotIDs := make([]string, len(got))
		for i, r := range got {
			gotIDs[i] = r.PolygonRecordID
		}

		if !equalStrings(gotIDs, wantIDs) {
			rt.Fatalf("result mismatch:\n got  = %v\n want = %v", gotIDs, wantIDs)
		}
	})
}
