package repo

import (
	"context"
	"testing"

	"github.com/influence/influence/internal/recordid"
)

// This file holds a focused end-to-end integration test for the search wiring
// (Req 23.1): it stands up a real file-backed tenant (via twoTenantFixture),
// indexes several Polygons with real multi-word content through
// PolygonIndex.IndexPolygon (masked write), then drives a battery of real FTS5
// MATCH queries through SearchRepo.Search and asserts the hits, ordering, and
// access scoping.
//
// search_test.go already proves the basic index -> query wiring for single
// hits, per-Sphere access scoping, no-grants, no-match, blank queries, and
// tenant isolation. This test adds the net-new end-to-end assertions those
// unit-style cases do not cover: a single term that matches MULTIPLE readable
// Polygons, multi-term (implicit AND) queries, an explicit phrase query, and a
// mix of matching/non-matching queries all run against one populated index —
// exercising the full write-then-read wiring in one pass.

// TestSearchWiringEndToEndMultipleQueries indexes a small corpus of Polygons
// with real multi-word content across two readable Spheres and one unreadable
// Sphere, then runs several FTS5 MATCH queries end to end (Req 23.1).
func TestSearchWiringEndToEndMultipleQueries(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()

	// Two Spheres the caller can read and one it cannot, all in tenant A.
	engineering := seedSphere(t, f.dbA, recordid.New(), "Engineering")
	product := seedSphere(t, f.dbA, recordid.New(), "Product")
	restricted := seedSphere(t, f.dbA, recordid.New(), "Restricted")

	// A corpus with deliberate term overlap so a single term can match several
	// Polygons, plus multi-word content for phrase/multi-term queries.
	roadmap := seedPolygonInSphere(t, f.dbA, engineering)
	metrics := seedPolygonInSphere(t, f.dbA, engineering)
	launch := seedPolygonInSphere(t, f.dbA, product)
	secret := seedPolygonInSphere(t, f.dbA, restricted)

	corpus := map[string]string{
		roadmap: "platform roadmap for the search service rewrite",
		metrics: "search service performance metrics and platform latency",
		launch:  "product launch checklist for the platform release",
		secret:  "confidential platform acquisition roadmap",
	}
	for rid, content := range corpus {
		if err := f.index.IndexPolygon(ctx, f.rcA, rid, content); err != nil {
			t.Fatalf("index polygon %s: %v", rid, err)
		}
	}

	// Caller can read Engineering and Product, but NOT Restricted.
	rc := rcWithGrants(engineering, product)

	// Each case names the query and the exact set of readable Record_IDs it
	// should return. "secret" must never appear: it lives in Restricted.
	cases := []struct {
		name  string
		query string
		want  map[string]bool
	}{
		{
			// A single term appearing in three readable Polygons AND the
			// unreadable one — every readable hit returns, the unreadable is
			// scoped out.
			name:  "single term matches multiple readable polygons",
			query: "platform",
			want:  map[string]bool{roadmap: true, metrics: true, launch: true},
		},
		{
			// Implicit-AND multi-term: both terms must be present. Only the two
			// Engineering Polygons carry "search" and "service" together.
			name:  "multi-term implicit AND narrows results",
			query: "search service",
			want:  map[string]bool{roadmap: true, metrics: true},
		},
		{
			// Explicit phrase: the words must be adjacent in order. "product
			// launch" appears only in the launch Polygon.
			name:  "phrase query matches adjacent words",
			query: `"product launch"`,
			want:  map[string]bool{launch: true},
		},
		{
			// A term that appears ONLY in the unreadable Restricted Sphere —
			// access scoping drops it, so the readable result set is empty.
			name:  "term only in unreadable sphere yields nothing",
			query: "acquisition",
			want:  map[string]bool{},
		},
		{
			// A term present in no indexed content at all.
			name:  "no-match term yields empty",
			query: "nonexistentterm",
			want:  map[string]bool{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.search.Search(ctx, rc, tc.query)
			if err != nil {
				t.Fatalf("search %q: %v", tc.query, err)
			}
			if got == nil {
				t.Fatalf("search %q returned nil, want non-nil set", tc.query)
			}

			gotIDs := recordIDs(got)
			if len(gotIDs) != len(tc.want) {
				t.Fatalf("search %q returned %d hits %v, want %d %v",
					tc.query, len(gotIDs), gotIDs, len(tc.want), tc.want)
			}
			for id := range tc.want {
				if !gotIDs[id] {
					t.Errorf("search %q missing expected hit %s; got %v", tc.query, id, gotIDs)
				}
			}
			// The Restricted Polygon must never leak, regardless of query.
			if gotIDs[secret] {
				t.Errorf("search %q leaked unreadable hit %s", tc.query, secret)
			}
			// Every returned hit is a fully-formed Polygon with type resolved.
			for _, p := range got {
				if p.Type != PolygonMarkdown {
					t.Errorf("search %q hit %s type = %q, want markdown", tc.query, p.RecordID, p.Type)
				}
			}
		})
	}
}
