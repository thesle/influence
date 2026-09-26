package repo

import (
	"context"
	"fmt"
	"testing"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 24: Search scoping and matching
//
// For all search queries and users, every returned Polygon both matches the
// query and resides in a Sphere the user can access, and no Polygon from an
// inaccessible Sphere is returned.
//
// Validates: Requirements 23.1
//
// The property runs over the real production tenant-routing path: a file-backed
// tenant behind a real ConnManager driven by a real Central_Directory registry
// (the searchFixture pattern from search_test.go), with a real polygon_fts FTS5
// index populated through PolygonIndex (masked content) and queried through
// SearchRepo. It reuses the search_test.go fixtures — searchFixture /
// newSearchFixture, seedSphere, seedPolygonInSphere, rcWithGrants — rather than
// redefining them.
//
// Each rapid run:
//
//   1. draws a small set of Spheres, each independently marked accessible or
//      not, and seeds them into tenant A;
//   2. draws a set of Polygons, each placed in one of those Spheres and given
//      content built from a small vocabulary of distinct lowercase-ASCII word
//      tokens; each Polygon is indexed through PolygonIndex (the masked write
//      side);
//   3. builds a RequestContext (via rcWithGrants) carrying read grants on
//      exactly the accessible Spheres;
//   4. draws a query word from the vocabulary, calls SearchRepo.Search, and
//      asserts the returned Record_ID set equals the independently computed
//      ground truth: exactly the Polygons whose content contains the query word
//      AND whose Sphere is accessible.
//
// Vocabulary and matching model. Content is a space-joined set of distinct
// words drawn from a fixed lowercase-ASCII alphabet vocabulary. FTS5's default
// unicode61 tokenizer splits on whitespace and folds ASCII case, so for these
// single-word lowercase tokens a MATCH on one word succeeds exactly when that
// word appears among the Polygon's content words. Reproducing "matches the
// query" in Go is therefore a simple set-membership test, letting the property
// assert the exact returned set rather than a weaker superset/subset relation.
func TestSearchPropertyScopingAndMatching(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()

	// Fixed vocabulary of distinct lowercase-ASCII word tokens. The unicode61
	// tokenizer treats each as a single indexable term and folds case, so
	// membership in a Polygon's word set is exactly FTS5's MATCH decision.
	vocab := []string{
		"alpha", "bravo", "charlie", "delta", "echo",
		"foxtrot", "golf", "hotel", "india", "juliet",
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Fresh tenant state per run: clear the index and the Polygons/Spheres so
		// each run's ground truth is computed against exactly its own set.
		if _, err := f.dbA.Exec(`DELETE FROM polygon_fts`); err != nil {
			rt.Fatalf("reset polygon_fts: %v", err)
		}
		if _, err := f.dbA.Exec(`DELETE FROM polygon`); err != nil {
			rt.Fatalf("reset polygon: %v", err)
		}
		if _, err := f.dbA.Exec(`DELETE FROM sphere`); err != nil {
			rt.Fatalf("reset sphere: %v", err)
		}

		// Draw a set of Spheres, each independently accessible or not. At least
		// one Sphere so Polygons have somewhere to live.
		sphereCount := rapid.IntRange(1, 4).Draw(rt, "sphereCount")
		type sphereInfo struct {
			surrogate  int64
			accessible bool
		}
		spheres := make([]sphereInfo, sphereCount)
		for i := range spheres {
			surrogate := seedSphere(t, f.dbA, recordid.New(), fmt.Sprintf("Sphere %d", i))
			accessible := rapid.Bool().Draw(rt, fmt.Sprintf("accessible_%d", i))
			spheres[i] = sphereInfo{surrogate: surrogate, accessible: accessible}
		}

		// Draw a set of Polygons, each placed in a drawn Sphere with a drawn,
		// non-empty subset of the vocabulary as its content words.
		polygonCount := rapid.IntRange(0, 8).Draw(rt, "polygonCount")
		type polygonInfo struct {
			rid            string
			sphereIndex    int
			words          map[string]bool
			sphereAccessed bool
		}
		polygons := make([]polygonInfo, polygonCount)
		for i := range polygons {
			sIdx := rapid.IntRange(0, sphereCount-1).Draw(rt, fmt.Sprintf("polySphere_%d", i))

			// Draw a non-empty subset of the vocabulary for this Polygon's words.
			words := map[string]bool{}
			var ordered []string
			for _, w := range vocab {
				if rapid.Bool().Draw(rt, fmt.Sprintf("poly_%d_word_%s", i, w)) {
					words[w] = true
					ordered = append(ordered, w)
				}
			}
			if len(ordered) == 0 {
				// Guarantee at least one word so the content is non-blank and the
				// Polygon is genuinely indexable.
				w := vocab[rapid.IntRange(0, len(vocab)-1).Draw(rt, fmt.Sprintf("poly_%d_forceWord", i))]
				words[w] = true
				ordered = append(ordered, w)
			}

			rid := seedPolygonInSphere(t, f.dbA, spheres[sIdx].surrogate)
			content := joinWords(ordered)
			if err := f.index.IndexPolygon(ctx, f.rcA, rid, content); err != nil {
				rt.Fatalf("index polygon %d: %v", i, err)
			}
			polygons[i] = polygonInfo{
				rid:            rid,
				sphereIndex:    sIdx,
				words:          words,
				sphereAccessed: spheres[sIdx].accessible,
			}
		}

		// Build the caller's RequestContext with read grants on exactly the
		// accessible Spheres, mirroring the effective grant map the middleware
		// hands downstream.
		var accessibleSurrogates []int64
		for _, s := range spheres {
			if s.accessible {
				accessibleSurrogates = append(accessibleSurrogates, s.surrogate)
			}
		}
		rc := rcWithGrants(accessibleSurrogates...)

		// Draw the query word from the vocabulary.
		queryWord := vocab[rapid.IntRange(0, len(vocab)-1).Draw(rt, "queryWord")]

		// Independently compute ground truth: the Polygons whose content contains
		// the query word AND that reside in an accessible Sphere.
		want := map[string]bool{}
		for _, p := range polygons {
			if p.sphereAccessed && p.words[queryWord] {
				want[p.rid] = true
			}
		}

		got, err := f.search.Search(ctx, rc, queryWord)
		if err != nil {
			rt.Fatalf("search %q: %v", queryWord, err)
		}
		gotSet := recordIDs(got)

		// Every returned Polygon must be a true match in an accessible Sphere:
		// never an inaccessible-Sphere hit, never a non-match.
		for _, p := range got {
			if !want[p.RecordID] {
				rt.Fatalf("search %q returned unexpected hit %s: it is either a non-match or in an inaccessible Sphere;\n want = %v\n got  = %v",
					queryWord, p.RecordID, keys(want), keys(gotSet))
			}
		}
		// Every true match in an accessible Sphere must be returned.
		for rid := range want {
			if !gotSet[rid] {
				rt.Fatalf("search %q missing expected hit %s (a match in an accessible Sphere);\n want = %v\n got  = %v",
					queryWord, rid, keys(want), keys(gotSet))
			}
		}
		// Exact-set: sizes agree once both containments hold.
		if len(gotSet) != len(want) {
			rt.Fatalf("search %q returned %d hits, want %d;\n want = %v\n got  = %v",
				queryWord, len(gotSet), len(want), keys(want), keys(gotSet))
		}
	})
}

// joinWords renders a set of content words as a single space-separated string
// suitable for indexing; the order is irrelevant to FTS5 term membership.
func joinWords(words []string) string {
	out := ""
	for i, w := range words {
		if i > 0 {
			out += " "
		}
		out += w
	}
	return out
}

// keys renders a Record_ID set as a slice for readable failure messages.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
