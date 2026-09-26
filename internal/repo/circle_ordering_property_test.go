package repo

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 5: Sphere display ordering
//
// For all sets of accessible Spheres and any per-user circled subset with a
// user-specified order, the displayed list is exactly the circled Spheres in
// the user's order followed by the remaining Spheres sorted ascending by
// case-insensitive name with Record_ID as tie-break.
//
// Validates: Requirements 7.3, 7.4, 7.5
//
// The property is exercised over the real production tenant-routing path: a
// file-backed tenant behind a real ConnManager driven by a real
// Central_Directory registry (the twoTenantFixture pattern from base_test.go).
// A single fixture is shared across all rapid runs; each run:
//
//   1. draws an arbitrary set of Spheres (names + freshly minted Record_IDs)
//      and seeds them into tenant A;
//   2. draws an arbitrary circled subset and an arbitrary order over that
//      subset, applying CircleRepo.Circle for each and then CircleRepo.Reorder
//      to persist the chosen order;
//   3. calls CircleRepo.ListOrdered and asserts the returned order equals the
//      independently computed expectation — [circled in the user's persisted
//      order] followed by [remainder sorted case-insensitively by name with the
//      canonical Record_ID as tie-break].
//
// Names are drawn from a small ASCII alphabet (letters, digits, space). SQLite's
// COLLATE NOCASE folds exactly the ASCII A–Z/a–z range, so restricting names to
// ASCII lets the Go expectation model NOCASE precisely via strings.ToLower —
// the case-fold and the Record_ID tie-break (a byte-wise comparison of the
// canonical UUID string, identical in Go and SQLite) are both reproduced
// faithfully. The alphabet includes upper/lower variants so names that differ
// only in case genuinely collide under NOCASE and force the Record_ID tie-break
// to decide their relative order.
func TestCirclePropertyDisplayOrdering(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	// ASCII-only alphabet: NOCASE folds exactly this range, so strings.ToLower
	// reproduces the collation. Mixed case makes case-insensitive collisions —
	// and thus the Record_ID tie-break — reachable.
	nameRunes := []rune{'a', 'A', 'b', 'B', 'm', 'M', 'z', 'Z', '0', '9', ' '}

	rapid.Check(t, func(rt *rapid.T) {
		// Draw an arbitrary set of spheres for this run.
		count := rapid.IntRange(0, 8).Draw(rt, "sphereCount")

		type sphere struct {
			rid  recordid.ID
			name string
		}
		spheres := make([]sphere, count)
		for i := range spheres {
			nlen := rapid.IntRange(1, 6).Draw(rt, "nameLen")
			rs := make([]rune, nlen)
			for j := range rs {
				rs[j] = nameRunes[rapid.IntRange(0, len(nameRunes)-1).Draw(rt, "rune")]
			}
			spheres[i] = sphere{rid: recordid.New(), name: string(rs)}
		}

		// Fresh tenant state per run: clear any spheres/circles seeded by a prior
		// run so each run's expectation is computed against exactly its own set.
		if _, err := f.dbA.Exec(`DELETE FROM user_circle`); err != nil {
			rt.Fatalf("reset user_circle: %v", err)
		}
		if _, err := f.dbA.Exec(`DELETE FROM sphere`); err != nil {
			rt.Fatalf("reset sphere: %v", err)
		}

		for _, s := range spheres {
			seedSphere(t, f.dbA, s.rid, s.name)
		}

		repo := &CircleRepo{Base: f.base}

		// Draw an arbitrary circled subset: for each sphere, decide whether it is
		// circled. The circled ones are collected preserving index order first,
		// then permuted into the user's chosen order below.
		var circledPool []sphere
		for _, s := range spheres {
			if len(spheres) > 0 && rapid.Bool().Draw(rt, "circle_"+s.rid.Canonical()) {
				circledPool = append(circledPool, s)
			}
		}

		// Draw an arbitrary order over the circled subset (a permutation).
		perm := rapid.Permutation(circledPool).Draw(rt, "circleOrder")

		// Apply the circling in the chosen order, then persist that exact order.
		for _, s := range perm {
			if err := repo.Circle(ctx, f.rcA, s.rid); err != nil {
				rt.Fatalf("circle %s (%q): %v", s.rid.Canonical(), s.name, err)
			}
		}
		if len(perm) > 0 {
			ids := make([]recordid.ID, len(perm))
			for i, s := range perm {
				ids[i] = s.rid
			}
			if err := repo.Reorder(ctx, f.rcA, ids); err != nil {
				rt.Fatalf("reorder: %v", err)
			}
		}

		// Independently compute the expected display order.
		circledSet := map[string]bool{}
		var wantNames []string
		for _, s := range perm {
			circledSet[s.rid.Canonical()] = true
			wantNames = append(wantNames, s.name)
		}

		var remainder []sphere
		for _, s := range spheres {
			if !circledSet[s.rid.Canonical()] {
				remainder = append(remainder, s)
			}
		}
		// Remainder sorted case-insensitively by name (NOCASE == ASCII ToLower on
		// this alphabet), Record_ID (canonical) as the stable tie-break.
		sort.SliceStable(remainder, func(i, j int) bool {
			li, lj := strings.ToLower(remainder[i].name), strings.ToLower(remainder[j].name)
			if li != lj {
				return li < lj
			}
			return remainder[i].rid.Canonical() < remainder[j].rid.Canonical()
		})
		for _, s := range remainder {
			wantNames = append(wantNames, s.name)
		}

		ordered, err := repo.ListOrdered(ctx, f.rcA)
		if err != nil {
			rt.Fatalf("list ordered: %v", err)
		}

		if gotNames := names(ordered); !equalStrings(gotNames, wantNames) {
			rt.Fatalf("display order mismatch:\n got  = %v\n want = %v\n circled(order) = %v",
				gotNames, wantNames, circledNames(perm, func(s sphere) string { return s.name }))
		}
	})
}

// circledNames renders the names of the circled slice in persisted order for a
// readable failure message; it is only used in the fatal path above.
func circledNames[T any](items []T, name func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = name(it)
	}
	return out
}
