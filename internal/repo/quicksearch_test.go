package repo

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// These tests exercise the Req 24 quick searches over the real production
// tenant-routing path (the twoTenantFixture from base_test.go): file-backed
// tenants behind a real ConnManager. They verify the "Polygons I've Created"
// filter and created_at DESC order (Req 24.1), the "Recently Viewed Polygons"
// distinct + newest-first + cap-50 behaviour (Req 24.2), per-Sphere
// accessibility filtering (Req 23.1), and empty result sets when nothing
// matches (Req 24.3).

// quickSearchFixture pairs the two-tenant fixture with a QuickSearch bound to
// the same ConnManager so quick searches run through the production path.
type quickSearchFixture struct {
	twoTenantFixture
	qs *QuickSearch
}

func newQuickSearchFixture(t *testing.T) quickSearchFixture {
	t.Helper()
	f := newTwoTenantFixture(t)
	return quickSearchFixture{
		twoTenantFixture: f,
		qs:               &QuickSearch{Base: f.base},
	}
}

// rcWithAccess builds a RequestContext for a user in tenant A that has read
// access to the given Sphere surrogate ids. Quick searches derive their
// accessible-Sphere set from the RequestContext grant map, so tests grant
// access by listing the surrogate sphere ids here.
func rcWithAccess(userID data.UserID, tenantID data.TenantID, sphereSurrogates ...int64) data.RequestContext {
	grants := make(map[data.SphereID]data.SphereGrant, len(sphereSurrogates))
	for _, id := range sphereSurrogates {
		grants[data.SphereID(id)] = data.SphereGrant{Access: data.AccessRead}
	}
	return data.NewRequestContext(userID, tenantID, nil, grants)
}

// seedPolygonAuthored inserts a Polygon row directly into a tenant DB under the
// given Sphere surrogate, authored by authorUserID, created at createdAt (ISO
// 8601), and returns its Record_ID and surrogate id.
func seedPolygonAuthored(t *testing.T, db *sql.DB, sphereSurrogate int64, authorUserID int64, createdAt string) (recordid.ID, int64) {
	t.Helper()
	rid := recordid.New()
	res, err := db.Exec(
		`INSERT INTO polygon (record_id, sphere_id, type, author_user_id, created_at, updated_at)
		 VALUES (?, ?, 'markdown', ?, ?, ?)`,
		rid.Canonical(), sphereSurrogate, authorUserID, createdAt, createdAt,
	)
	if err != nil {
		t.Fatalf("seed polygon: %v", err)
	}
	surrogate, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return rid, surrogate
}

// seedView inserts a VIEW_LOG row directly for the given user, polygon
// surrogate, and view time (ISO 8601).
func seedView(t *testing.T, db *sql.DB, userID int64, polygonSurrogate int64, viewedAt string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO view_log (user_id, polygon_id, viewed_at) VALUES (?, ?, ?)`,
		userID, polygonSurrogate, viewedAt,
	); err != nil {
		t.Fatalf("seed view: %v", err)
	}
}

// TestPolygonsICreatedFiltersAndOrders confirms PolygonsICreated returns only
// the caller's authored Polygons in accessible Spheres, newest-created first
// (Req 24.1).
func TestPolygonsICreatedFiltersAndOrders(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	const other = 99
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")

	// Three Polygons I authored, created at increasing times.
	oldRID, _ := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-01-01T00:00:00Z")
	midRID, _ := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-02-01T00:00:00Z")
	newRID, _ := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-03-01T00:00:00Z")
	// One authored by someone else — must not appear.
	seedPolygonAuthored(t, f.dbA, sphereSurrogate, other, "2024-04-01T00:00:00Z")

	rc := rcWithAccess(me, 1, sphereSurrogate)
	got, err := f.qs.PolygonsICreated(ctx, rc)
	if err != nil {
		t.Fatalf("polygons i created: %v", err)
	}
	wantOrder := []string{newRID.Canonical(), midRID.Canonical(), oldRID.Canonical()}
	if len(got) != len(wantOrder) {
		t.Fatalf("result len = %d, want %d (%+v)", len(got), len(wantOrder), got)
	}
	for i, want := range wantOrder {
		if got[i].PolygonRecordID != want {
			t.Errorf("position %d = %q, want %q", i, got[i].PolygonRecordID, want)
		}
		if got[i].SphereRecordID != sphereRID.Canonical() {
			t.Errorf("position %d sphere = %q, want %q", i, got[i].SphereRecordID, sphereRID.Canonical())
		}
	}
}

// TestPolygonsICreatedExcludesInaccessibleSpheres confirms a Polygon the caller
// authored but which sits in a Sphere they cannot access is excluded (Req 23.1).
func TestPolygonsICreatedExcludesInaccessibleSpheres(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	accessibleRID := recordid.New()
	accessible := seedSphere(t, f.dbA, accessibleRID, "Accessible")
	blockedRID := recordid.New()
	blocked := seedSphere(t, f.dbA, blockedRID, "Blocked")

	visibleRID, _ := seedPolygonAuthored(t, f.dbA, accessible, me, "2024-01-01T00:00:00Z")
	seedPolygonAuthored(t, f.dbA, blocked, me, "2024-02-01T00:00:00Z")

	// Grant read only on the accessible Sphere.
	rc := rcWithAccess(me, 1, accessible)
	got, err := f.qs.PolygonsICreated(ctx, rc)
	if err != nil {
		t.Fatalf("polygons i created: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("result len = %d, want 1 (%+v)", len(got), got)
	}
	if got[0].PolygonRecordID != visibleRID.Canonical() {
		t.Errorf("got %q, want %q", got[0].PolygonRecordID, visibleRID.Canonical())
	}
}

// TestPolygonsICreatedEmptyWhenNothingMatches confirms an empty (non-nil) slice
// is returned when the caller authored nothing accessible (Req 24.3).
func TestPolygonsICreatedEmptyWhenNothingMatches(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")
	// Authored by someone else only.
	seedPolygonAuthored(t, f.dbA, sphereSurrogate, 99, "2024-01-01T00:00:00Z")

	rc := rcWithAccess(me, 1, sphereSurrogate)
	got, err := f.qs.PolygonsICreated(ctx, rc)
	if err != nil {
		t.Fatalf("polygons i created: %v", err)
	}
	if got == nil {
		t.Fatal("result is nil, want empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("result len = %d, want 0 (%+v)", len(got), got)
	}
}

// TestPolygonsICreatedEmptyWithNoAccessibleSpheres confirms that a caller with
// no Sphere grants gets an empty set even if they authored Polygons (Req 23.1,
// 24.3).
func TestPolygonsICreatedEmptyWithNoAccessibleSpheres(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")
	seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-01-01T00:00:00Z")

	// rcA from the fixture has no grants at all.
	got, err := f.qs.PolygonsICreated(ctx, f.rcA)
	if err != nil {
		t.Fatalf("polygons i created: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("result len = %d, want 0 (no accessible spheres)", len(got))
	}
}

// TestRecentlyViewedDistinctAndNewestFirst confirms a Polygon viewed multiple
// times appears once, ordered by its most recent view, newest first (Req 24.2).
func TestRecentlyViewedDistinctAndNewestFirst(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")

	aRID, aSur := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-01-01T00:00:00Z")
	bRID, bSur := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-01-01T00:00:00Z")

	// A viewed twice; its most recent view is later than B's single view, so A
	// should sort ahead of B despite B being viewed after A's first view.
	seedView(t, f.dbA, me, aSur, "2024-05-01T00:00:00Z")
	seedView(t, f.dbA, me, bSur, "2024-06-01T00:00:00Z")
	seedView(t, f.dbA, me, aSur, "2024-07-01T00:00:00Z")

	rc := rcWithAccess(me, 1, sphereSurrogate)
	got, err := f.qs.RecentlyViewed(ctx, rc)
	if err != nil {
		t.Fatalf("recently viewed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("result len = %d, want 2 distinct (%+v)", len(got), got)
	}
	if got[0].PolygonRecordID != aRID.Canonical() {
		t.Errorf("first = %q, want A %q", got[0].PolygonRecordID, aRID.Canonical())
	}
	if got[1].PolygonRecordID != bRID.Canonical() {
		t.Errorf("second = %q, want B %q", got[1].PolygonRecordID, bRID.Canonical())
	}
	if got[0].When != "2024-07-01T00:00:00Z" {
		t.Errorf("A's ordering time = %q, want most recent 2024-07-01", got[0].When)
	}
}

// TestRecentlyViewedCapsAtFifty confirms the list is capped at the 50 most
// recently viewed Polygons (Req 24.2).
func TestRecentlyViewedCapsAtFifty(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// 60 distinct Polygons, each viewed once, at strictly increasing times.
	var newestRID recordid.ID
	for i := 0; i < 60; i++ {
		rid, sur := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, base.Format(time.RFC3339))
		viewedAt := base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		seedView(t, f.dbA, me, sur, viewedAt)
		newestRID = rid // the last one has the latest view time
	}

	rc := rcWithAccess(me, 1, sphereSurrogate)
	got, err := f.qs.RecentlyViewed(ctx, rc)
	if err != nil {
		t.Fatalf("recently viewed: %v", err)
	}
	if len(got) != recentlyViewedCap {
		t.Fatalf("result len = %d, want cap %d", len(got), recentlyViewedCap)
	}
	// The most recently viewed Polygon must be first even after the cap.
	if got[0].PolygonRecordID != newestRID.Canonical() {
		t.Errorf("first after cap = %q, want newest %q", got[0].PolygonRecordID, newestRID.Canonical())
	}
}

// TestRecentlyViewedExcludesInaccessibleSpheres confirms a viewed Polygon in a
// Sphere the caller cannot access is excluded (Req 23.1).
func TestRecentlyViewedExcludesInaccessibleSpheres(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	accessibleRID := recordid.New()
	accessible := seedSphere(t, f.dbA, accessibleRID, "Accessible")
	blockedRID := recordid.New()
	blocked := seedSphere(t, f.dbA, blockedRID, "Blocked")

	visibleRID, visSur := seedPolygonAuthored(t, f.dbA, accessible, me, "2024-01-01T00:00:00Z")
	_, blkSur := seedPolygonAuthored(t, f.dbA, blocked, me, "2024-01-01T00:00:00Z")

	seedView(t, f.dbA, me, visSur, "2024-05-01T00:00:00Z")
	seedView(t, f.dbA, me, blkSur, "2024-06-01T00:00:00Z")

	rc := rcWithAccess(me, 1, accessible)
	got, err := f.qs.RecentlyViewed(ctx, rc)
	if err != nil {
		t.Fatalf("recently viewed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("result len = %d, want 1 (%+v)", len(got), got)
	}
	if got[0].PolygonRecordID != visibleRID.Canonical() {
		t.Errorf("got %q, want %q", got[0].PolygonRecordID, visibleRID.Canonical())
	}
}

// TestRecentlyViewedOnlyOwnViews confirms the list reflects the caller's own
// VIEW_LOG entries and not another user's views (Req 24.2).
func TestRecentlyViewedOnlyOwnViews(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	const other = 99
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")

	mineRID, mineSur := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-01-01T00:00:00Z")
	_, theirsSur := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-01-01T00:00:00Z")

	seedView(t, f.dbA, me, mineSur, "2024-05-01T00:00:00Z")
	seedView(t, f.dbA, other, theirsSur, "2024-06-01T00:00:00Z")

	rc := rcWithAccess(me, 1, sphereSurrogate)
	got, err := f.qs.RecentlyViewed(ctx, rc)
	if err != nil {
		t.Fatalf("recently viewed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("result len = %d, want 1 (%+v)", len(got), got)
	}
	if got[0].PolygonRecordID != mineRID.Canonical() {
		t.Errorf("got %q, want my viewed polygon %q", got[0].PolygonRecordID, mineRID.Canonical())
	}
}

// TestRecentlyViewedEmptyWhenNothingViewed confirms an empty (non-nil) slice is
// returned when the caller has viewed nothing (Req 24.3).
func TestRecentlyViewedEmptyWhenNothingViewed(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")
	seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-01-01T00:00:00Z")

	rc := rcWithAccess(me, 1, sphereSurrogate)
	got, err := f.qs.RecentlyViewed(ctx, rc)
	if err != nil {
		t.Fatalf("recently viewed: %v", err)
	}
	if got == nil {
		t.Fatal("result is nil, want empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("result len = %d, want 0 (%+v)", len(got), got)
	}
}

// TestRecordViewFeedsRecentlyViewed confirms RecordView writes a VIEW_LOG entry
// that surfaces the Polygon in RecentlyViewed, and that a strictly-later view
// moves the Polygon to the front (Req 24.2).
func TestRecordViewFeedsRecentlyViewed(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	const me = 10
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")
	aRID, _ := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-01-01T00:00:00Z")
	bRID, _ := seedPolygonAuthored(t, f.dbA, sphereSurrogate, me, "2024-01-01T00:00:00Z")

	rc := rcWithAccess(me, 1, sphereSurrogate)

	// Recording two views (which land at the current time) makes both Polygons
	// appear in the list. Because RecordView stamps second-granularity ISO 8601
	// UTC times, back-to-back calls may tie; the assertion here is only that
	// both views are surfaced, not their relative order.
	if err := f.qs.RecordView(ctx, rc, aRID.Canonical()); err != nil {
		t.Fatalf("record view a: %v", err)
	}
	if err := f.qs.RecordView(ctx, rc, bRID.Canonical()); err != nil {
		t.Fatalf("record view b: %v", err)
	}

	got, err := f.qs.RecentlyViewed(ctx, rc)
	if err != nil {
		t.Fatalf("recently viewed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("result len = %d, want 2 (%+v)", len(got), got)
	}
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.PolygonRecordID] = true
	}
	if !seen[aRID.Canonical()] || !seen[bRID.Canonical()] {
		t.Fatalf("recorded views not both surfaced: %+v", got)
	}

	// Backdate every existing view so a fresh RecordView of A is strictly the
	// most recent, then confirm A moves to the front of the list.
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if _, err := f.dbA.Exec(`UPDATE view_log SET viewed_at = ?`, past); err != nil {
		t.Fatalf("backdate views: %v", err)
	}
	if err := f.qs.RecordView(ctx, rc, aRID.Canonical()); err != nil {
		t.Fatalf("re-record view a: %v", err)
	}

	got, err = f.qs.RecentlyViewed(ctx, rc)
	if err != nil {
		t.Fatalf("recently viewed after re-record: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("result len after re-record = %d, want 2 (%+v)", len(got), got)
	}
	if got[0].PolygonRecordID != aRID.Canonical() {
		t.Errorf("first after re-record = %q, want A %q", got[0].PolygonRecordID, aRID.Canonical())
	}
}

// TestRecordViewCrossTenantNotAccessible confirms RecordView on a Polygon owned
// by another tenant is not accessible and writes nothing (Req 1.6).
func TestRecordViewCrossTenantNotAccessible(t *testing.T) {
	f := newQuickSearchFixture(t)
	ctx := context.Background()

	// Polygon exists only in tenant B.
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbB, sphereRID, "B Sphere")
	polyRID, _ := seedPolygonAuthored(t, f.dbB, sphereSurrogate, 20, "2024-01-01T00:00:00Z")

	rc := rcWithAccess(10, 1, sphereSurrogate)
	if err := f.qs.RecordView(ctx, rc, polyRID.Canonical()); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant record view: got %v, want ErrNotAccessible", err)
	}

	// No VIEW_LOG row was written in either tenant.
	var n int
	if err := f.dbA.QueryRow(`SELECT COUNT(*) FROM view_log`).Scan(&n); err != nil {
		t.Fatalf("count view_log A: %v", err)
	}
	if n != 0 {
		t.Errorf("tenant A view_log rows = %d, want 0", n)
	}
	if err := f.dbB.QueryRow(`SELECT COUNT(*) FROM view_log`).Scan(&n); err != nil {
		t.Fatalf("count view_log B: %v", err)
	}
	if n != 0 {
		t.Errorf("tenant B view_log rows = %d, want 0", n)
	}
}
