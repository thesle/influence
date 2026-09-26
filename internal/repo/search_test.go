package repo

import (
	"context"
	"testing"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"

	"database/sql"
)

// These tests exercise search query scoping (Req 23.1, 23.3) over the real
// production tenant-routing path (twoTenantFixture): file-backed tenants behind
// a real ConnManager with a real polygon_fts FTS5 table created by
// MigrateTenant. They confirm a search returns only Polygons that both match the
// query AND reside in a Sphere the caller can read, and that a query matching
// nothing readable yields an empty result set.

// searchFixture pairs the two-tenant fixture with a SearchRepo and a PolygonIndex
// (bound to the test masker) so a test can index content and then search it,
// mirroring how the write and read sides cooperate in production.
type searchFixture struct {
	twoTenantFixture
	search *SearchRepo
	index  *PolygonIndex
}

func newSearchFixture(t *testing.T) searchFixture {
	t.Helper()
	f := newTwoTenantFixture(t)
	return searchFixture{
		twoTenantFixture: f,
		search:           &SearchRepo{Base: f.base},
		index:            &PolygonIndex{Base: f.base, mask: testMask},
	}
}

// seedPolygonInSphere inserts a markdown Polygon into the given Sphere surrogate
// in tenant A and returns its Record_ID.
func seedPolygonInSphere(t *testing.T, db *sql.DB, sphereSurrogate int64) string {
	t.Helper()
	rid := recordid.New().Canonical()
	_, err := db.Exec(
		`INSERT INTO polygon (record_id, sphere_id, type, author_user_id, created_at, updated_at)
		 VALUES (?, ?, 'markdown', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		rid, sphereSurrogate,
	)
	if err != nil {
		t.Fatalf("seed polygon: %v", err)
	}
	return rid
}

// rcWithGrants builds a tenant-A RequestContext carrying read grants on the
// given Sphere surrogates, mirroring the effective grant map the middleware
// hands downstream.
func rcWithGrants(sphereSurrogates ...int64) data.RequestContext {
	grants := make(map[data.SphereID]data.SphereGrant, len(sphereSurrogates))
	for _, s := range sphereSurrogates {
		grants[data.SphereID(s)] = data.SphereGrant{Access: data.AccessRead}
	}
	return data.NewRequestContext(10, 1, nil, grants)
}

func recordIDs(polys []Polygon) map[string]bool {
	out := make(map[string]bool, len(polys))
	for _, p := range polys {
		out[p.RecordID] = true
	}
	return out
}

// TestSearchReturnsMatchesInAccessibleSphere confirms a query returns a Polygon
// whose masked content matches AND that resides in a Sphere the caller can read
// (Req 23.1).
func TestSearchReturnsMatchesInAccessibleSphere(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()

	sphere := seedSphere(t, f.dbA, recordid.New(), "Accessible")
	rid := seedPolygonInSphere(t, f.dbA, sphere)
	if err := f.index.IndexPolygon(ctx, f.rcA, rid, "quarterly revenue report"); err != nil {
		t.Fatalf("index polygon: %v", err)
	}

	rc := rcWithGrants(sphere)
	got, err := f.search.Search(ctx, rc, "revenue")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].RecordID != rid {
		t.Fatalf("search = %v, want single hit %s", got, rid)
	}
	// The hit is a fully-formed Polygon with its owning Sphere resolved.
	if got[0].Type != PolygonMarkdown {
		t.Errorf("hit type = %q, want markdown", got[0].Type)
	}
}

// TestSearchExcludesMatchesInInaccessibleSphere confirms a matching Polygon in a
// Sphere the caller cannot read is excluded from results even though its content
// matches the query (Req 23.1).
func TestSearchExcludesMatchesInInaccessibleSphere(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()

	accessible := seedSphere(t, f.dbA, recordid.New(), "Accessible")
	restricted := seedSphere(t, f.dbA, recordid.New(), "Restricted")

	openRID := seedPolygonInSphere(t, f.dbA, accessible)
	secretRID := seedPolygonInSphere(t, f.dbA, restricted)

	// Both Polygons match the same term.
	if err := f.index.IndexPolygon(ctx, f.rcA, openRID, "shared platform roadmap"); err != nil {
		t.Fatalf("index open polygon: %v", err)
	}
	if err := f.index.IndexPolygon(ctx, f.rcA, secretRID, "shared platform secrets"); err != nil {
		t.Fatalf("index secret polygon: %v", err)
	}

	// Caller can read only the accessible Sphere.
	rc := rcWithGrants(accessible)
	got, err := f.search.Search(ctx, rc, "platform")
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	ids := recordIDs(got)
	if !ids[openRID] {
		t.Errorf("search missing accessible hit %s; got %v", openRID, got)
	}
	if ids[secretRID] {
		t.Errorf("search leaked inaccessible hit %s; got %v", secretRID, got)
	}
	if len(got) != 1 {
		t.Errorf("search returned %d hits, want 1", len(got))
	}
}

// TestSearchNoGrantsExcludesEverything confirms a caller with no Sphere grants
// gets an empty set even when Polygons match the query — access scoping drops
// every hit (Req 23.1, 23.3).
func TestSearchNoGrantsExcludesEverything(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()

	sphere := seedSphere(t, f.dbA, recordid.New(), "Sphere")
	rid := seedPolygonInSphere(t, f.dbA, sphere)
	if err := f.index.IndexPolygon(ctx, f.rcA, rid, "orphaned content here"); err != nil {
		t.Fatalf("index polygon: %v", err)
	}

	// rcA carries no sphere grants at all.
	got, err := f.search.Search(ctx, f.rcA, "orphaned")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("search with no grants = %v, want empty", got)
	}
}

// TestSearchNoMatchReturnsEmpty confirms a query that matches no indexed content
// returns an empty result set (Req 23.3).
func TestSearchNoMatchReturnsEmpty(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()

	sphere := seedSphere(t, f.dbA, recordid.New(), "Sphere")
	rid := seedPolygonInSphere(t, f.dbA, sphere)
	if err := f.index.IndexPolygon(ctx, f.rcA, rid, "alpha bravo charlie"); err != nil {
		t.Fatalf("index polygon: %v", err)
	}

	rc := rcWithGrants(sphere)
	got, err := f.search.Search(ctx, rc, "zulu")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got == nil {
		t.Fatal("search returned nil, want non-nil empty set")
	}
	if len(got) != 0 {
		t.Errorf("search no-match = %v, want empty", got)
	}
}

// TestSearchBlankQueryReturnsEmpty confirms a blank/whitespace-only query is
// treated as matching nothing rather than erroring on an empty FTS MATCH
// (Req 23.3).
func TestSearchBlankQueryReturnsEmpty(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()

	sphere := seedSphere(t, f.dbA, recordid.New(), "Sphere")
	rid := seedPolygonInSphere(t, f.dbA, sphere)
	if err := f.index.IndexPolygon(ctx, f.rcA, rid, "some content"); err != nil {
		t.Fatalf("index polygon: %v", err)
	}

	rc := rcWithGrants(sphere)
	for _, q := range []string{"", "   ", "\t\n"} {
		got, err := f.search.Search(ctx, rc, q)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(got) != 0 {
			t.Errorf("search blank %q = %v, want empty", q, got)
		}
	}
}

// TestSearchScopedToOwnTenant confirms search runs only against the caller's own
// tenant DB: content indexed in tenant A is invisible to a tenant-B search even
// with a matching query (Req 1.4, 1.5).
func TestSearchScopedToOwnTenant(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()

	sphere := seedSphere(t, f.dbA, recordid.New(), "Sphere A")
	rid := seedPolygonInSphere(t, f.dbA, sphere)
	if err := f.index.IndexPolygon(ctx, f.rcA, rid, "tenant a private content"); err != nil {
		t.Fatalf("index polygon: %v", err)
	}

	// Tenant B searches with a grant on the same surrogate id — but its own
	// tenant file has no such Polygon indexed, so nothing matches.
	rcB := data.NewRequestContext(20, 2, nil, map[data.SphereID]data.SphereGrant{
		data.SphereID(sphere): {Access: data.AccessRead},
	})
	got, err := f.search.Search(ctx, rcB, "private")
	if err != nil {
		t.Fatalf("search tenant B: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("cross-tenant search = %v, want empty", got)
	}
}
