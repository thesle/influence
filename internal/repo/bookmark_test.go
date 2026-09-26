package repo

import (
	"context"
	"errors"
	"testing"

	"database/sql"

	"github.com/influence/influence/internal/recordid"
)

// These tests exercise the Bookmark repository over the real production tenant-
// routing path (the twoTenantFixture from base_test.go): file-backed tenants
// behind a real ConnManager. They verify add idempotence (Req 26.1, 26.2),
// remove (Req 26.5), grouping by owning Sphere (Req 26.3), and the empty-state
// indicator (Req 26.4).
//
// They reuse newTwoTenantFixture and seedSphere (base_test.go). Bookmarking
// needs a Polygon's Record_ID, which the shared seedPolygon helper does not
// return, so seedPolygonWithRecordID below seeds a Polygon and returns both its
// surrogate id and Record_ID. It is a distinct helper and does not redefine
// seedPolygon.

// bookmarkFixture pairs the two-tenant fixture with a BookmarkRepo bound to the
// same ConnManager so bookmark operations run through the production path.
type bookmarkFixture struct {
	twoTenantFixture
	repo *BookmarkRepo
}

func newBookmarkFixture(t *testing.T) bookmarkFixture {
	t.Helper()
	f := newTwoTenantFixture(t)
	return bookmarkFixture{
		twoTenantFixture: f,
		repo:             &BookmarkRepo{Base: f.base},
	}
}

// seedPolygonWithRecordID inserts a Polygon under the given sphere surrogate id
// and returns both its surrogate id and its Record_ID (the latter is what the
// bookmark API addresses Polygons by).
func seedPolygonWithRecordID(t *testing.T, db *sql.DB, sphereID int64) (int64, recordid.ID) {
	t.Helper()
	rid := recordid.New()
	res, err := db.Exec(
		`INSERT INTO polygon (record_id, sphere_id, type, author_user_id, created_at, updated_at)
		 VALUES (?, ?, 'markdown', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		rid.Canonical(), sphereID,
	)
	if err != nil {
		t.Fatalf("seed polygon: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("polygon last insert id: %v", err)
	}
	return id, rid
}

func countBookmarks(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bookmark`).Scan(&n); err != nil {
		t.Fatalf("count bookmarks: %v", err)
	}
	return n
}

// TestAddBookmarkIsIdempotent confirms bookmarking a Polygon adds it (Req 26.1)
// and that a repeat add leaves the list unchanged (Req 26.2).
func TestAddBookmarkIsIdempotent(t *testing.T) {
	f := newBookmarkFixture(t)
	ctx := context.Background()

	sphereSurrogate := seedSphere(t, f.dbA, recordid.New(), "Sphere One")
	_, polyRID := seedPolygonWithRecordID(t, f.dbA, sphereSurrogate)

	if err := f.repo.AddBookmark(ctx, f.rcA, polyRID.Canonical()); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if n := countBookmarks(t, f.dbA); n != 1 {
		t.Fatalf("after first add, bookmark count = %d, want 1", n)
	}

	// A second add of the same Polygon must be a no-op (Req 26.2).
	if err := f.repo.AddBookmark(ctx, f.rcA, polyRID.Canonical()); err != nil {
		t.Fatalf("second add: %v", err)
	}
	if n := countBookmarks(t, f.dbA); n != 1 {
		t.Errorf("after repeat add, bookmark count = %d, want 1 (unchanged)", n)
	}
}

// TestRemoveBookmark confirms removing a Bookmark deletes it from the list
// (Req 26.5), and that removing one that is not present is a harmless no-op.
func TestRemoveBookmark(t *testing.T) {
	f := newBookmarkFixture(t)
	ctx := context.Background()

	sphereSurrogate := seedSphere(t, f.dbA, recordid.New(), "Sphere One")
	_, polyRID := seedPolygonWithRecordID(t, f.dbA, sphereSurrogate)

	if err := f.repo.AddBookmark(ctx, f.rcA, polyRID.Canonical()); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := f.repo.RemoveBookmark(ctx, f.rcA, polyRID.Canonical()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if n := countBookmarks(t, f.dbA); n != 0 {
		t.Errorf("after remove, bookmark count = %d, want 0", n)
	}

	// Removing again (nothing bookmarked) must not error.
	if err := f.repo.RemoveBookmark(ctx, f.rcA, polyRID.Canonical()); err != nil {
		t.Errorf("remove of absent bookmark: %v", err)
	}

	list, err := f.repo.ListBookmarks(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if !list.Empty || len(list.Groups) != 0 {
		t.Errorf("after remove, list = %+v, want empty", list)
	}
}

// TestListBookmarksGroupedBySphere confirms bookmarks are partitioned by owning
// Sphere, with each bookmarked Polygon appearing under exactly its own Sphere
// (Req 26.3).
func TestListBookmarksGroupedBySphere(t *testing.T) {
	f := newBookmarkFixture(t)
	ctx := context.Background()

	sphereARID := recordid.New()
	sphereBRID := recordid.New()
	sphereA := seedSphere(t, f.dbA, sphereARID, "Alpha")
	sphereB := seedSphere(t, f.dbA, sphereBRID, "Bravo")

	// Two Polygons in Alpha, one in Bravo — all bookmarked by the caller.
	_, a1 := seedPolygonWithRecordID(t, f.dbA, sphereA)
	_, a2 := seedPolygonWithRecordID(t, f.dbA, sphereA)
	_, b1 := seedPolygonWithRecordID(t, f.dbA, sphereB)

	for _, rid := range []recordid.ID{a1, a2, b1} {
		if err := f.repo.AddBookmark(ctx, f.rcA, rid.Canonical()); err != nil {
			t.Fatalf("add %s: %v", rid.Canonical(), err)
		}
	}

	list, err := f.repo.ListBookmarks(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if list.Empty {
		t.Fatal("list reported empty with bookmarks present")
	}
	if len(list.Groups) != 2 {
		t.Fatalf("group count = %d, want 2", len(list.Groups))
	}

	// Groups are ordered by Sphere name: Alpha then Bravo.
	if list.Groups[0].Sphere.RecordID != sphereARID {
		t.Errorf("group 0 sphere = %s, want Alpha", list.Groups[0].Sphere.RecordID.Canonical())
	}
	if got := len(list.Groups[0].Polygons); got != 2 {
		t.Errorf("Alpha polygon count = %d, want 2", got)
	}
	if list.Groups[1].Sphere.RecordID != sphereBRID {
		t.Errorf("group 1 sphere = %s, want Bravo", list.Groups[1].Sphere.RecordID.Canonical())
	}
	if got := len(list.Groups[1].Polygons); got != 1 {
		t.Errorf("Bravo polygon count = %d, want 1", got)
	}

	// Every returned Polygon reports the Sphere it was grouped under.
	for _, g := range list.Groups {
		for _, p := range g.Polygons {
			if p.SphereRecordID != g.Sphere.RecordID.Canonical() {
				t.Errorf("polygon %s grouped under %s but reports sphere %s",
					p.RecordID, g.Sphere.RecordID.Canonical(), p.SphereRecordID)
			}
		}
	}
}

// TestListBookmarksEmptyState confirms a User with no bookmarks gets an empty
// result flagged for the empty-bookmarks indicator (Req 26.4).
func TestListBookmarksEmptyState(t *testing.T) {
	f := newBookmarkFixture(t)
	ctx := context.Background()

	list, err := f.repo.ListBookmarks(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !list.Empty {
		t.Errorf("Empty = false, want true for a user with no bookmarks")
	}
	if len(list.Groups) != 0 {
		t.Errorf("group count = %d, want 0", len(list.Groups))
	}
}

// TestBookmarkCrossTenantNotAccessible confirms bookmarking a Polygon that
// lives only in another tenant is rejected as not accessible and changes
// nothing (Req 1.6).
func TestBookmarkCrossTenantNotAccessible(t *testing.T) {
	f := newBookmarkFixture(t)
	ctx := context.Background()

	// Polygon exists only in tenant B.
	sphereB := seedSphere(t, f.dbB, recordid.New(), "In B")
	_, polyRID := seedPolygonWithRecordID(t, f.dbB, sphereB)

	if err := f.repo.AddBookmark(ctx, f.rcA, polyRID.Canonical()); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant add: got %v, want ErrNotAccessible", err)
	}
	if n := countBookmarks(t, f.dbA); n != 0 {
		t.Errorf("tenant A bookmark count = %d, want 0", n)
	}
	if n := countBookmarks(t, f.dbB); n != 0 {
		t.Errorf("tenant B bookmark count = %d, want 0", n)
	}
}
