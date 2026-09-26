package repo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/influence/influence/internal/recordid"
)

// These tests exercise Polygon Create/Get/List over the real production tenant-
// routing path (the twoTenantFixture from base_test.go): file-backed tenants
// behind a real ConnManager. They verify type validity (Req 10.1, 10.2, 10.3),
// storage within a Sphere with an assigned tenant-unique Record_ID (Req 10.4,
// 10.5), persistence rollback that leaves the Sphere unchanged on rejection
// (Req 10.6), and Folder_Polygon containment (Req 10.7).
//
// They reuse seedSphere (base_test.go), countPolygons (facet_test.go), and
// newTwoTenantFixture; those helpers are defined elsewhere and are not
// redefined here.

// polygonFixture pairs the two-tenant fixture with a PolygonRepo bound to the
// same ConnManager so Polygon operations run through the production path.
type polygonFixture struct {
	twoTenantFixture
	repo *PolygonRepo
}

func newPolygonFixture(t *testing.T) polygonFixture {
	t.Helper()
	f := newTwoTenantFixture(t)
	return polygonFixture{
		twoTenantFixture: f,
		repo:             &PolygonRepo{Base: f.base},
	}
}

// TestPolygonCreateEachValidType confirms all four supported types are accepted
// and stored, each with an assigned Record_ID (Req 10.1, 10.2, 10.4, 10.5).
func TestPolygonCreateEachValidType(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	for _, typ := range []PolygonType{PolygonMarkdown, PolygonFolder, PolygonTabular, PolygonWhiteboard} {
		got, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), typ, 10, CreateOptions{})
		if err != nil {
			t.Fatalf("create %q: %v", typ, err)
		}
		if got.Type != typ {
			t.Errorf("type = %q, want %q", got.Type, typ)
		}
		if got.SphereRecordID != sphereRID.Canonical() {
			t.Errorf("sphere = %q, want %q", got.SphereRecordID, sphereRID.Canonical())
		}
		if _, err := recordid.Parse(got.RecordID); err != nil {
			t.Errorf("assigned record id %q is not valid: %v", got.RecordID, err)
		}
	}

	if n := countPolygons(t, f.dbA); n != 4 {
		t.Errorf("polygon count = %d, want 4", n)
	}
	if n := countPolygons(t, f.dbB); n != 0 {
		t.Errorf("tenant B polygon count = %d, want 0", n)
	}
}

// TestPolygonCreateRecordsAuthorAndRoundTrips confirms the author and type are
// stored and the Polygon round-trips through Get (Req 10.2, 10.5).
func TestPolygonCreateRecordsAuthorAndRoundTrips(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	created, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 42, CreateOptions{})
	if err != nil {
		t.Fatalf("create polygon: %v", err)
	}
	if created.AuthorUserID != 42 {
		t.Errorf("author = %d, want 42", created.AuthorUserID)
	}

	got, err := f.repo.Get(ctx, f.rcA, created.RecordID)
	if err != nil {
		t.Fatalf("get polygon: %v", err)
	}
	if got.RecordID != created.RecordID || got.Type != PolygonMarkdown || got.AuthorUserID != 42 {
		t.Errorf("get = %+v, want id %s type markdown author 42", got, created.RecordID)
	}
	if got.SphereRecordID != sphereRID.Canonical() {
		t.Errorf("get sphere = %q, want %q", got.SphereRecordID, sphereRID.Canonical())
	}
}

// TestPolygonCreateInvalidTypeRejected confirms a type outside the four
// supported values is rejected with ErrValidation and creates nothing
// (Req 10.3, 10.6).
func TestPolygonCreateInvalidTypeRejected(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	before := countPolygons(t, f.dbA)
	for _, bad := range []PolygonType{"", "diagram", "Markdown", "MARKDOWN", "page"} {
		_, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), bad, 10, CreateOptions{})
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid type %q: got %v, want ErrValidation", bad, err)
		}
	}
	if after := countPolygons(t, f.dbA); after != before {
		t.Errorf("invalid-type rejection changed polygon count: before=%d after=%d", before, after)
	}
}

// TestPolygonCreateUnknownSphereNotAccessible confirms creating under a Sphere
// not in the caller's tenant returns ErrNotAccessible and writes nothing
// (Req 1.6, 10.6).
func TestPolygonCreateUnknownSphereNotAccessible(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	// Sphere exists only in tenant B.
	sphereRID := recordid.New()
	seedSphere(t, f.dbB, sphereRID, "Only In B")

	before := countPolygons(t, f.dbA)
	_, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{})
	if !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant sphere: got %v, want ErrNotAccessible", err)
	}
	if after := countPolygons(t, f.dbA); after != before {
		t.Errorf("rejected create changed polygon count: before=%d after=%d", before, after)
	}
}

// TestPolygonFolderContainsPolygons confirms a Folder_Polygon may contain other
// Polygons, including other folders, via parent_polygon_id (Req 10.7).
func TestPolygonFolderContainsPolygons(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	folder, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonFolder, 10, CreateOptions{})
	if err != nil {
		t.Fatalf("create folder: %v", err)
	}

	// A markdown page inside the folder.
	page, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{ParentRecordID: &folder.RecordID})
	if err != nil {
		t.Fatalf("create page in folder: %v", err)
	}
	if page.ParentRecordID == nil || *page.ParentRecordID != folder.RecordID {
		t.Errorf("page parent = %v, want %q", page.ParentRecordID, folder.RecordID)
	}

	// A nested folder inside the folder (folders may contain folders).
	nested, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonFolder, 10, CreateOptions{ParentRecordID: &folder.RecordID})
	if err != nil {
		t.Fatalf("create nested folder: %v", err)
	}

	// Round-trip the parent linkage through Get.
	gotPage, err := f.repo.Get(ctx, f.rcA, page.RecordID)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	if gotPage.ParentRecordID == nil || *gotPage.ParentRecordID != folder.RecordID {
		t.Errorf("stored page parent = %v, want %q", gotPage.ParentRecordID, folder.RecordID)
	}
	gotNested, err := f.repo.Get(ctx, f.rcA, nested.RecordID)
	if err != nil {
		t.Fatalf("get nested: %v", err)
	}
	if gotNested.ParentRecordID == nil || *gotNested.ParentRecordID != folder.RecordID {
		t.Errorf("stored nested parent = %v, want %q", gotNested.ParentRecordID, folder.RecordID)
	}
}

// TestPolygonNonFolderParentRejected confirms a non-folder parent is rejected
// with ErrValidation and writes nothing — only folders may contain Polygons
// (Req 10.7).
func TestPolygonNonFolderParentRejected(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	page, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{})
	if err != nil {
		t.Fatalf("create page: %v", err)
	}

	before := countPolygons(t, f.dbA)
	_, err = f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{ParentRecordID: &page.RecordID})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("non-folder parent: got %v, want ErrValidation", err)
	}
	if after := countPolygons(t, f.dbA); after != before {
		t.Errorf("rejected create changed polygon count: before=%d after=%d", before, after)
	}
}

// TestPolygonListInSphere confirms List returns exactly the Polygons of the
// named Sphere, and excludes Polygons of other Spheres.
func TestPolygonListInSphere(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")
	otherSphere := recordid.New()
	seedSphere(t, f.dbA, otherSphere, "Other")

	if _, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{}); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if _, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonTabular, 10, CreateOptions{}); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if _, err := f.repo.Create(ctx, f.rcA, otherSphere.Canonical(), PolygonMarkdown, 10, CreateOptions{}); err != nil {
		t.Fatalf("create in other sphere: %v", err)
	}

	list, err := f.repo.List(ctx, f.rcA, sphereRID.Canonical())
	if err != nil {
		t.Fatalf("list polygons: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	for _, p := range list {
		if p.SphereRecordID != sphereRID.Canonical() {
			t.Errorf("listed polygon in wrong sphere: %q", p.SphereRecordID)
		}
	}
}

// TestPolygonGetCrossTenantNotAccessible confirms a Polygon Record_ID from
// another tenant is not accessible (Req 1.6).
func TestPolygonGetCrossTenantNotAccessible(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbB, sphereRID, "B Sphere")
	created, err := f.repo.Create(ctx, f.rcB, sphereRID.Canonical(), PolygonMarkdown, 20, CreateOptions{})
	if err != nil {
		t.Fatalf("create in B: %v", err)
	}

	if _, err := f.repo.Get(ctx, f.rcA, created.RecordID); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant get: got %v, want ErrNotAccessible", err)
	}
}

// TestPolygonCreateWithFacet confirms a Polygon may be filed under a Facet, and
// the Facet linkage round-trips through Get.
func TestPolygonCreateWithFacet(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")
	facetSurrogate := seedFacet(t, f.dbA, sphereSurrogate, 0, "Roads")

	var facetCanonical string
	if err := f.dbA.QueryRow(`SELECT record_id FROM facet WHERE id = ?`, facetSurrogate).Scan(&facetCanonical); err != nil {
		t.Fatalf("resolve facet record id: %v", err)
	}

	created, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{FacetRecordID: &facetCanonical})
	if err != nil {
		t.Fatalf("create with facet: %v", err)
	}
	if created.FacetRecordID == nil || *created.FacetRecordID != facetCanonical {
		t.Errorf("facet = %v, want %q", created.FacetRecordID, facetCanonical)
	}

	got, err := f.repo.Get(ctx, f.rcA, created.RecordID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FacetRecordID == nil || *got.FacetRecordID != facetCanonical {
		t.Errorf("stored facet = %v, want %q", got.FacetRecordID, facetCanonical)
	}
}

// TestPolygonCreateRecordsAuthorAndTimestamps confirms Create records the
// authoring User's id and stamps created_at/updated_at with equal ISO 8601 UTC
// timestamps (Req 18.1).
func TestPolygonCreateRecordsAuthorAndTimestamps(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	created, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 42, CreateOptions{})
	if err != nil {
		t.Fatalf("create polygon: %v", err)
	}
	if created.AuthorUserID != 42 {
		t.Errorf("author = %d, want 42", created.AuthorUserID)
	}
	// Both timestamps are set, equal on create, and parse as ISO 8601 (RFC3339)
	// in UTC (Req 18.1).
	if created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("timestamps not recorded: created=%q updated=%q", created.CreatedAt, created.UpdatedAt)
	}
	if created.CreatedAt != created.UpdatedAt {
		t.Errorf("on create updated_at should equal created_at: created=%q updated=%q", created.CreatedAt, created.UpdatedAt)
	}
	ct, err := time.Parse(time.RFC3339, created.CreatedAt)
	if err != nil {
		t.Fatalf("created_at %q not ISO 8601: %v", created.CreatedAt, err)
	}
	if _, off := ct.Zone(); off != 0 {
		t.Errorf("created_at %q is not UTC (offset %d)", created.CreatedAt, off)
	}
}

// TestPolygonTouchUpdatesLastEdit confirms Touch refreshes only updated_at to a
// current ISO 8601 UTC timestamp on edit, leaving author and created_at intact
// (Req 18.2).
func TestPolygonTouchUpdatesLastEdit(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	created, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 42, CreateOptions{})
	if err != nil {
		t.Fatalf("create polygon: %v", err)
	}

	// Force the stored created_at/updated_at back in time so a Touch produces a
	// strictly later last-edit timestamp regardless of clock resolution.
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if _, err := f.dbA.Exec(`UPDATE polygon SET created_at = ?, updated_at = ? WHERE record_id = ?`, past, past, created.RecordID); err != nil {
		t.Fatalf("backdate polygon: %v", err)
	}

	edited, err := f.repo.Touch(ctx, f.rcA, created.RecordID)
	if err != nil {
		t.Fatalf("touch polygon: %v", err)
	}
	if edited.CreatedAt != past {
		t.Errorf("created_at changed on edit: got %q, want %q", edited.CreatedAt, past)
	}
	if edited.AuthorUserID != 42 {
		t.Errorf("author changed on edit: got %d, want 42", edited.AuthorUserID)
	}
	et, err := time.Parse(time.RFC3339, edited.UpdatedAt)
	if err != nil {
		t.Fatalf("updated_at %q not ISO 8601: %v", edited.UpdatedAt, err)
	}
	if _, off := et.Zone(); off != 0 {
		t.Errorf("updated_at %q is not UTC (offset %d)", edited.UpdatedAt, off)
	}
	pastT, _ := time.Parse(time.RFC3339, past)
	if !et.After(pastT) {
		t.Errorf("updated_at %q should be after backdated %q", edited.UpdatedAt, past)
	}
}

// TestPolygonTouchCrossTenantNotAccessible confirms Touch on a Polygon owned by
// another tenant is not accessible and changes nothing (Req 1.6, 18.2).
func TestPolygonTouchCrossTenantNotAccessible(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbB, sphereRID, "B Sphere")
	created, err := f.repo.Create(ctx, f.rcB, sphereRID.Canonical(), PolygonMarkdown, 20, CreateOptions{})
	if err != nil {
		t.Fatalf("create in B: %v", err)
	}

	if _, err := f.repo.Touch(ctx, f.rcA, created.RecordID); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant touch: got %v, want ErrNotAccessible", err)
	}
}
