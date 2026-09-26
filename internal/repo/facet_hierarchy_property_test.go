package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 7: Facet hierarchy integrity
//
// For all Facet forests within a Sphere and any reparent operation, if the
// proposed parent is the Facet itself, one of its descendants, or a Facet in a
// different Sphere the operation is rejected and the hierarchy is unchanged;
// otherwise the resulting hierarchy still has every Facet with at most one
// parent and no Facet as its own ancestor.
//
// Validates: Requirements 9.1, 9.2, 9.3
//
// The property runs over the real production tenant-routing path: a file-backed
// tenant behind a real ConnManager driven by a real Central_Directory registry
// (the facetFixture pattern from facet_test.go), with a FacetRepo bound to that
// ConnManager. Two Spheres are seeded into tenant A so cross-Sphere reparents
// (Req 9.3) can be exercised alongside same-Sphere ones.
//
// Each rapid run drives an arbitrary sequence of operations against the forest:
//
//   - create: add a Facet under an existing same-Sphere parent (or top level),
//     or occasionally under a cross-Sphere parent (rejected up front with
//     ErrParentSphereMismatch, leaving the forest untouched);
//   - reparent: move a Facet under another Facet, to top level, to itself, or
//     under one of its own descendants.
//
// After every operation the test reads the entire facet table straight from the
// tenant handle and asserts the two forest invariants (Req 9.2): every Facet has
// at most one parent (guaranteed by the single parent_facet_id column) and no
// Facet is its own ancestor (verified by walking each Facet's parent chain and
// detecting any repeat). Whenever a self/descendant/cross-Sphere reparent is
// attempted, the test confirms it is rejected with ErrHierarchy and that the
// full parent map is byte-for-byte identical before and after (Req 9.1, 9.3).
func TestFacetHierarchyIntegrityProperty(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	// Two Spheres in tenant A. Facets are created in sphere A1; sphere A2 exists
	// only to host a cross-Sphere parent so reparent (Req 9.3) can be exercised.
	sphere1 := recordid.New()
	sphere2 := recordid.New()
	seedSphere(t, f.dbA, sphere1, "Sphere One")
	seedSphere(t, f.dbA, sphere2, "Sphere Two")

	// A single top-level Facet in the other Sphere, used as an always-invalid
	// cross-Sphere reparent target.
	crossSphereParent, err := f.repo.Create(ctx, f.rcA, sphere2.Canonical(), nil, "Cross")
	if err != nil {
		t.Fatalf("seed cross-sphere facet: %v", err)
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Fresh forest per run: clear every Facet in sphere 1 so runs do not
		// accumulate state. The cross-Sphere parent in sphere 2 is preserved.
		if _, err := f.dbA.Exec(`DELETE FROM facet WHERE record_id != ?`, crossSphereParent.RecordID); err != nil {
			rt.Fatalf("reset facets: %v", err)
		}

		// facetIDs tracks the Record_IDs of Facets created in sphere 1, so
		// operations can pick real targets. It starts empty each run.
		var facetIDs []string

		// A short-to-moderate sequence of operations keeps each run cheap while
		// still building non-trivial trees and exercising every reject path.
		ops := rapid.IntRange(1, 12).Draw(rt, "opCount")
		for i := 0; i < ops; i++ {
			// Snapshot the parent map before the operation so a rejected op can
			// be proven to change nothing.
			before := readParentMap(rt, f.dbA)

			if len(facetIDs) == 0 || rapid.Bool().Draw(rt, fmt.Sprintf("create_%d", i)) {
				// CREATE. Most creates go into sphere 1 under an existing parent
				// (or top level). A fraction target the cross-Sphere parent, which
				// must be rejected with ErrParentSphereMismatch and write nothing.
				crossSphere := rapid.Bool().Draw(rt, fmt.Sprintf("createCross_%d", i))
				if crossSphere {
					parent := crossSphereParent.RecordID
					_, err := f.repo.Create(ctx, f.rcA, sphere1.Canonical(), &parent, fmt.Sprintf("f%d", i))
					if !errors.Is(err, ErrParentSphereMismatch) {
						rt.Fatalf("cross-sphere create: got %v, want ErrParentSphereMismatch", err)
					}
					assertParentMapUnchanged(rt, f.dbA, before, "rejected cross-sphere create")
					continue
				}

				var parent *string
				if len(facetIDs) > 0 && rapid.Bool().Draw(rt, fmt.Sprintf("createUnderParent_%d", i)) {
					p := facetIDs[rapid.IntRange(0, len(facetIDs)-1).Draw(rt, fmt.Sprintf("createParent_%d", i))]
					parent = &p
				}
				created, err := f.repo.Create(ctx, f.rcA, sphere1.Canonical(), parent, fmt.Sprintf("f%d", i))
				if err != nil {
					rt.Fatalf("create facet: %v", err)
				}
				facetIDs = append(facetIDs, created.RecordID)
			} else {
				// REPARENT. Pick a Facet to move, then pick a target that is one
				// of: another Facet, top level, itself, a descendant of itself, or
				// the cross-Sphere Facet. The invalid targets must be rejected.
				facet := facetIDs[rapid.IntRange(0, len(facetIDs)-1).Draw(rt, fmt.Sprintf("moveFacet_%d", i))]

				kind := rapid.IntRange(0, 4).Draw(rt, fmt.Sprintf("reparentKind_%d", i))
				switch kind {
				case 0:
					// Move under another (arbitrary) Facet. This may itself be a
					// self/descendant target, so classify against the current tree
					// and assert accordingly.
					target := facetIDs[rapid.IntRange(0, len(facetIDs)-1).Draw(rt, fmt.Sprintf("moveTarget_%d", i))]
					err := f.repo.Reparent(ctx, f.rcA, facet, &target)
					if target == facet || isDescendant(rt, f.dbA, before, facet, target) {
						if !errors.Is(err, ErrHierarchy) {
							rt.Fatalf("self/descendant reparent (facet=%s target=%s): got %v, want ErrHierarchy", facet, target, err)
						}
						assertParentMapUnchanged(rt, f.dbA, before, "rejected self/descendant reparent")
					} else if err != nil {
						rt.Fatalf("valid reparent (facet=%s target=%s): %v", facet, target, err)
					}
				case 1:
					// Move to top level. Always valid.
					if err := f.repo.Reparent(ctx, f.rcA, facet, nil); err != nil {
						rt.Fatalf("reparent to top level: %v", err)
					}
				case 2:
					// Self parent — always rejected, hierarchy unchanged (Req 9.1).
					if err := f.repo.Reparent(ctx, f.rcA, facet, &facet); !errors.Is(err, ErrHierarchy) {
						rt.Fatalf("self reparent: got %v, want ErrHierarchy", err)
					}
					assertParentMapUnchanged(rt, f.dbA, before, "rejected self reparent")
				case 3:
					// Descendant parent — rejected when a descendant exists; if the
					// Facet is a leaf there is none, so skip to keep the draw honest.
					desc := pickDescendant(rt, f.dbA, before, facet)
					if desc == "" {
						continue
					}
					if err := f.repo.Reparent(ctx, f.rcA, facet, &desc); !errors.Is(err, ErrHierarchy) {
						rt.Fatalf("descendant reparent (facet=%s desc=%s): got %v, want ErrHierarchy", facet, desc, err)
					}
					assertParentMapUnchanged(rt, f.dbA, before, "rejected descendant reparent")
				case 4:
					// Cross-Sphere parent — always rejected (Req 9.3).
					target := crossSphereParent.RecordID
					if err := f.repo.Reparent(ctx, f.rcA, facet, &target); !errors.Is(err, ErrHierarchy) {
						rt.Fatalf("cross-sphere reparent: got %v, want ErrHierarchy", err)
					}
					assertParentMapUnchanged(rt, f.dbA, before, "rejected cross-sphere reparent")
				}
			}

			// After every operation the forest invariants must hold (Req 9.2).
			assertForestAcyclic(rt, f.dbA)
		}
	})
}

// readParentMap returns, for tenant A, a map from every Facet's Record_ID to its
// parent's Record_ID ("" for top-level). It is the canonical snapshot used to
// prove a rejected operation changed nothing and to reason about ancestry.
func readParentMap(rt *rapid.T, db *sql.DB) map[string]string {
	rt.Helper()
	rows, err := db.Query(
		`SELECT f.record_id, p.record_id
		 FROM facet f
		 LEFT JOIN facet p ON p.id = f.parent_facet_id`)
	if err != nil {
		rt.Fatalf("read parent map: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var id string
		var parent sql.NullString
		if err := rows.Scan(&id, &parent); err != nil {
			rt.Fatalf("scan parent map: %v", err)
		}
		if parent.Valid {
			out[id] = parent.String
		} else {
			out[id] = ""
		}
	}
	if err := rows.Err(); err != nil {
		rt.Fatalf("iterate parent map: %v", err)
	}
	return out
}

// assertParentMapUnchanged fails the run if the current parent map differs from
// the snapshot taken before a rejected operation.
func assertParentMapUnchanged(rt *rapid.T, db *sql.DB, before map[string]string, what string) {
	rt.Helper()
	after := readParentMap(rt, db)
	if len(after) != len(before) {
		rt.Fatalf("%s changed facet count: before=%d after=%d", what, len(before), len(after))
	}
	for id, parent := range before {
		if after[id] != parent {
			rt.Fatalf("%s changed parent of %s: before=%q after=%q", what, id, parent, after[id])
		}
	}
}

// isDescendant reports whether target is target==facet or lies in the subtree
// rooted at facet, per the parent map — i.e. whether making target the parent of
// facet would create a cycle. It walks target's ancestor chain looking for facet.
func isDescendant(rt *rapid.T, db *sql.DB, m map[string]string, facet, target string) bool {
	rt.Helper()
	if target == facet {
		return true
	}
	seen := map[string]struct{}{}
	cur := target
	for cur != "" {
		if cur == facet {
			return true
		}
		if _, ok := seen[cur]; ok {
			rt.Fatalf("cycle detected while classifying descendant from %s", target)
		}
		seen[cur] = struct{}{}
		cur = m[cur]
	}
	return false
}

// pickDescendant returns the Record_ID of some Facet strictly below facet in the
// parent map, or "" if facet is a leaf. It scans the map for any Facet whose
// ancestor chain reaches facet.
func pickDescendant(rt *rapid.T, db *sql.DB, m map[string]string, facet string) string {
	rt.Helper()
	for id := range m {
		if id == facet {
			continue
		}
		if isDescendant(rt, db, m, facet, id) {
			return id
		}
	}
	return ""
}

// assertForestAcyclic verifies the two forest invariants (Req 9.2) directly from
// the tenant table: every Facet has at most one parent — guaranteed structurally
// by the single parent_facet_id column and the map representation — and no Facet
// is its own ancestor, verified by walking each Facet's parent chain and failing
// if any Facet repeats before the chain terminates at a top-level Facet.
func assertForestAcyclic(rt *rapid.T, db *sql.DB) {
	rt.Helper()
	m := readParentMap(rt, db)
	for start := range m {
		seen := map[string]struct{}{}
		cur := start
		for cur != "" {
			if _, ok := seen[cur]; ok {
				rt.Fatalf("facet %s is its own ancestor: cycle reached %s", start, cur)
			}
			seen[cur] = struct{}{}
			next, ok := m[cur]
			if !ok {
				// Parent references a Facet not in the map — should never happen
				// for an intact forest.
				rt.Fatalf("facet %s references missing parent %s", cur, next)
			}
			cur = next
		}
	}
}
