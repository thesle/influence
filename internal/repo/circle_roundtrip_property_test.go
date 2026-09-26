package repo

import (
	"context"
	"testing"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 6: Circling idempotence and un-circle round-trip
//
// For all circled lists and any Sphere, circling a Sphere already circled
// leaves the list unchanged, and circling then un-circling a Sphere returns the
// list to its prior state with that Sphere back in the alphabetical remainder.
//
// Validates: Requirements 7.2, 7.6
//
// The property is exercised over the real production tenant-routing path: a
// file-backed tenant behind a real ConnManager driven by a real
// Central_Directory registry (the twoTenantFixture pattern from base_test.go).
// A single fixture seeds a fixed set of Spheres once; each rapid run drives a
// sequence of circle/un-circle operations against the caller's per-user list
// and checks the two guarantees:
//
//   - Idempotence (Req 7.2): after the target Sphere is circled, circling it
//     again leaves both the persisted circled surrogate set (order included)
//     and the full display listing byte-for-byte unchanged.
//
//   - Un-circle round-trip (Req 7.6): capturing the display listing before a
//     Sphere is circled, then circling and immediately un-circling it, returns
//     the listing to exactly that prior state — which, because the Sphere was
//     not circled beforehand, places it back in the alphabetical remainder in
//     the same position it held before.
//
// The Sphere names deliberately collide under case-insensitive comparison and
// vary in case so the alphabetical remainder (Req 7.5) is non-trivial: the
// round-trip must restore the exact case-insensitive/Record_ID-tie-broken order,
// not merely "some" ordering.
func TestCirclePropertyIdempotenceAndRoundTrip(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	// Seed a fixed set of Spheres in tenant A. Names collide under NOCASE and
	// span cases so the alphabetical remainder ordering is a real constraint.
	seedNames := []string{"Apple", "banana", "Cherry", "apple", "BANANA", "date"}
	rids := make([]recordid.ID, len(seedNames))
	for i, name := range seedNames {
		rid := recordid.New()
		rids[i] = rid
		seedSphere(t, f.dbA, rid, name)
	}

	repo := &CircleRepo{Base: f.base}
	userID := int64(f.rcA.UserID())

	orderedNames := func(rt *rapid.T) []string {
		spheres, err := repo.ListOrdered(ctx, f.rcA)
		if err != nil {
			rt.Fatalf("list ordered: %v", err)
		}
		return names(spheres)
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Pick a target Sphere from the seeded set.
		idx := rapid.IntRange(0, len(rids)-1).Draw(rt, "target")
		target := rids[idx]

		// Ensure a clean starting point for this run: the target must not be
		// circled beforehand, so its "prior state" is the alphabetical remainder.
		if err := repo.UnCircle(ctx, f.rcA, target); err != nil {
			rt.Fatalf("pre-clean uncircle: %v", err)
		}

		// Capture the prior state (before circling the target).
		beforeList := orderedNames(rt)
		beforeCircled := circledSurrogates(t, f, userID)

		// Circle the target once.
		if err := repo.Circle(ctx, f.rcA, target); err != nil {
			rt.Fatalf("circle: %v", err)
		}
		afterFirstList := orderedNames(rt)
		afterFirstCircled := circledSurrogates(t, f, userID)

		// Idempotence (Req 7.2): circling an already-circled Sphere leaves both
		// the persisted circled set and the full display listing unchanged.
		if err := repo.Circle(ctx, f.rcA, target); err != nil {
			rt.Fatalf("second circle: %v", err)
		}
		if got := orderedNames(rt); !equalStrings(got, afterFirstList) {
			rt.Fatalf("idempotence violated: display order changed on re-circle: %v -> %v",
				afterFirstList, got)
		}
		if got := circledSurrogates(t, f, userID); !equalInts(got, afterFirstCircled) {
			rt.Fatalf("idempotence violated: circled set changed on re-circle: %v -> %v",
				afterFirstCircled, got)
		}

		// Un-circle round-trip (Req 7.6): circle-then-uncircle returns the list
		// to its prior state, with the target back in the alphabetical remainder.
		if err := repo.UnCircle(ctx, f.rcA, target); err != nil {
			rt.Fatalf("uncircle: %v", err)
		}
		afterList := orderedNames(rt)
		afterCircled := circledSurrogates(t, f, userID)

		if !equalStrings(afterList, beforeList) {
			rt.Fatalf("round-trip violated: display order not restored: before=%v after=%v",
				beforeList, afterList)
		}
		if !equalInts(afterCircled, beforeCircled) {
			rt.Fatalf("round-trip violated: circled set not restored: before=%v after=%v",
				beforeCircled, afterCircled)
		}
	})
}

// equalInts reports whether two int64 slices are equal element-wise.
func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
