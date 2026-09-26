package repo

import (
	"context"
	"errors"
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"
)

// Feature: influence, Property 4: Sphere name validation and uniqueness
//
// For all candidate Sphere names, creation succeeds if and only if the name is
// 1–100 characters and not a duplicate of an existing Sphere name within the
// same tenant; rejected names leave the tenant's Sphere set unchanged.
//
// Validates: Requirements 6.4
//
// The property is exercised over the real production tenant-routing path: two
// file-backed tenants behind a real ConnManager driven by a real
// Central_Directory registry (the twoTenantFixture pattern from base_test.go).
// A single fixture is shared across all rapid runs; each run draws a candidate
// name plus a target tenant and applies SphereRepo.Create, checking the
// accept/reject decision against the length-and-uniqueness predicate and
// asserting the tenant's Sphere set changes by exactly one row on accept and by
// zero rows on reject.
//
// Names are drawn to straddle the interesting frontiers:
//   - the empty string (rejected: too short);
//   - lengths spanning 1..102 runes, so the 1 and 100 boundaries and the 101+
//     over-length region are all sampled (validation counts runes, not bytes);
//   - a small alphabet including a multi-byte rune, so rune-vs-byte length is
//     distinguished and exact-name duplicate matching is meaningful.
//
// Uniqueness is per-tenant: the generator sometimes targets tenant A and
// sometimes tenant B, and each tenant tracks its own accepted-name set. The
// same name is therefore accepted in each tenant independently — a name already
// present in A is still a first occurrence (and accepted) in B — which directly
// exercises the "within the same tenant" clause.
func TestSpherePropertyNameValidationAndUniqueness(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	// Per-tenant record of names already accepted, so the expected decision can
	// be computed independently of the repo. Uniqueness is scoped per tenant, so
	// each tenant keeps its own set.
	accepted := map[int]map[string]bool{
		1: {},
		2: {},
	}

	// A rune alphabet including a multi-byte rune ("é") so a name can be short in
	// runes yet long in bytes; validation is defined on rune length (Req 6.2,
	// 6.4).
	nameRunes := []rune{'a', 'b', 'Z', '0', ' ', 'é'}

	rapid.Check(t, func(rt *rapid.T) {
		// Draw a length spanning the empty string, both boundaries, and the
		// over-length region.
		n := rapid.IntRange(0, 102).Draw(rt, "nameLen")
		rs := make([]rune, n)
		for i := range rs {
			rs[i] = nameRunes[rapid.IntRange(0, len(nameRunes)-1).Draw(rt, "rune")]
		}
		name := string(rs)

		// Choose which tenant to create in; uniqueness must be independent
		// across the two.
		tenantID := rapid.SampledFrom([]int{1, 2}).Draw(rt, "tenant")
		var rc = f.rcA
		var db = f.dbA
		if tenantID == 2 {
			rc = f.rcB
			db = f.dbB
		}

		// Expected decision: accepted iff length in runes is 1..100 AND the exact
		// name is not already present in this tenant.
		length := utf8.RuneCountInString(name)
		lengthOK := length >= 1 && length <= sphereNameMaxLen
		duplicate := accepted[tenantID][name]
		wantAccept := lengthOK && !duplicate

		before := countRows(t, db, "sphere")

		_, err := (&SphereRepo{Base: f.base}).Create(ctx, rc, name)

		after := countRows(t, db, "sphere")

		if wantAccept {
			if err != nil {
				rt.Fatalf("name %q (len %d, tenant %d) should be accepted, got error: %v",
					name, length, tenantID, err)
			}
			if after != before+1 {
				rt.Fatalf("accepted name %q should add exactly one row: before=%d after=%d",
					name, before, after)
			}
			accepted[tenantID][name] = true
		} else {
			if !errors.Is(err, ErrValidation) {
				rt.Fatalf("name %q (len %d, tenant %d, duplicate=%v) should be rejected with ErrValidation, got: %v",
					name, length, tenantID, duplicate, err)
			}
			if after != before {
				rt.Fatalf("rejected name %q must leave the Sphere set unchanged: before=%d after=%d",
					name, before, after)
			}
		}
	})
}
