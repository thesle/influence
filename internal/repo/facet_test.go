package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/influence/influence/internal/recordid"
)

// facetFixture extends the two-tenant fixture with a FacetRepo bound to the same
// ConnManager, so Facet operations run through the exact production tenant-
// routing path.
type facetFixture struct {
	twoTenantFixture
	repo *FacetRepo
}

func newFacetFixture(t *testing.T) facetFixture {
	t.Helper()
	f := newTwoTenantFixture(t)
	return facetFixture{
		twoTenantFixture: f,
		repo:             &FacetRepo{Base: f.base},
	}
}

// countFacets and countPolygons read row counts directly from a tenant handle
// so tests can assert on cascade behavior independent of the repository.
func countFacets(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM facet`).Scan(&n); err != nil {
		t.Fatalf("count facets: %v", err)
	}
	return n
}

func countPolygons(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM polygon`).Scan(&n); err != nil {
		t.Fatalf("count polygons: %v", err)
	}
	return n
}

// seedFacetPolygon inserts a minimal Polygon row assigned to the given facet
// within a tenant DB and returns its surrogate id. It is distinct from the
// Sphere test's seedPolygon helper because the cascade tests need to control
// both facet_id and parent_polygon_id.
func seedFacetPolygon(t *testing.T, db *sql.DB, sphereSurrogate int64, facetSurrogate sql.NullInt64, parent sql.NullInt64) int64 {
	t.Helper()
	rid := recordid.New()
	res, err := db.Exec(
		`INSERT INTO polygon (record_id, sphere_id, facet_id, parent_polygon_id, type, author_user_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'markdown', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		rid.Canonical(), sphereSurrogate, facetSurrogate, parent,
	)
	if err != nil {
		t.Fatalf("seed polygon: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("polygon last insert id: %v", err)
	}
	return id
}

// TestCreateFacetStoresInSphere confirms a valid create writes a Facet with a
// fresh Record_ID belonging to the named Sphere (Req 8.3, 8.4).
func TestCreateFacetStoresInSphere(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	got, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "Roads")
	if err != nil {
		t.Fatalf("create facet: %v", err)
	}
	if got.Name != "Roads" {
		t.Errorf("name = %q, want %q", got.Name, "Roads")
	}
	if got.SphereRecordID != sphereRID.Canonical() {
		t.Errorf("sphere = %q, want %q", got.SphereRecordID, sphereRID.Canonical())
	}
	if got.ParentRecordID != nil {
		t.Errorf("parent = %v, want nil", *got.ParentRecordID)
	}
	if _, err := recordid.Parse(got.RecordID); err != nil {
		t.Errorf("assigned record id %q is not a valid Record_ID: %v", got.RecordID, err)
	}

	// Round-trips through Get.
	fetched, err := f.repo.Get(ctx, f.rcA, got.RecordID)
	if err != nil {
		t.Fatalf("get facet: %v", err)
	}
	if fetched != got {
		t.Errorf("get = %+v, want %+v", fetched, got)
	}
}

// TestCreateChildFacet confirms a Facet may contain child Facets (Req 8.2): a
// create under a parent in the same Sphere links to that parent.
func TestCreateChildFacet(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	parent, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "Parent")
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), &parent.RecordID, "Child")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if child.ParentRecordID == nil || *child.ParentRecordID != parent.RecordID {
		t.Errorf("child parent = %v, want %q", child.ParentRecordID, parent.RecordID)
	}
}

// TestCreateFacetRejectsEmptyName confirms an empty name is a validation error
// and writes nothing (Req 8.5).
func TestCreateFacetRejectsEmptyName(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	_, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "")
	if !errors.Is(err, ErrFacetValidation) {
		t.Fatalf("empty name: got %v, want ErrFacetValidation", err)
	}
	if n := countFacets(t, f.dbA); n != 0 {
		t.Errorf("facet count = %d after rejected create, want 0", n)
	}
}

// TestCreateFacetRejectsOversizeName confirms a >100-character name is a
// validation error, while exactly 100 characters is accepted (Req 8.4, 8.5).
func TestCreateFacetRejectsOversizeName(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	oversize := strings.Repeat("x", 101)
	if _, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, oversize); !errors.Is(err, ErrFacetValidation) {
		t.Fatalf("oversize name: got %v, want ErrFacetValidation", err)
	}

	exactly := strings.Repeat("y", 100)
	if _, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, exactly); err != nil {
		t.Fatalf("100-char name should be accepted: %v", err)
	}
}

// TestCreateFacetUnknownSphereNotAccessible confirms creating under a Sphere
// that is not in the caller's tenant returns ErrNotAccessible (Req 1.6).
func TestCreateFacetUnknownSphereNotAccessible(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	// Sphere exists only in tenant B.
	sphereRID := recordid.New()
	seedSphere(t, f.dbB, sphereRID, "Only In B")

	_, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "Roads")
	if !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant sphere: got %v, want ErrNotAccessible", err)
	}
}

// TestCreateFacetParentDifferentSphereRejected confirms a parent Facet in a
// different Sphere is rejected and no row is written.
func TestCreateFacetParentDifferentSphereRejected(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereA := recordid.New()
	sphereB := recordid.New()
	seedSphere(t, f.dbA, sphereA, "Sphere A")
	seedSphere(t, f.dbA, sphereB, "Sphere B")

	parentInB, err := f.repo.Create(ctx, f.rcA, sphereB.Canonical(), nil, "Parent In B")
	if err != nil {
		t.Fatalf("create parent in B: %v", err)
	}

	before := countFacets(t, f.dbA)
	_, err = f.repo.Create(ctx, f.rcA, sphereA.Canonical(), &parentInB.RecordID, "Child")
	if !errors.Is(err, ErrParentSphereMismatch) {
		t.Fatalf("cross-sphere parent: got %v, want ErrParentSphereMismatch", err)
	}
	if after := countFacets(t, f.dbA); after != before {
		t.Errorf("facet count changed by rejected create: before=%d after=%d", before, after)
	}
}

// TestListFacetsInSphere confirms List returns exactly the Facets of the named
// Sphere, ordered by name.
func TestListFacetsInSphere(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	otherSphere := recordid.New()
	seedSphere(t, f.dbA, otherSphere, "Other")

	if _, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "Beta"); err != nil {
		t.Fatalf("create Beta: %v", err)
	}
	if _, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "Alpha"); err != nil {
		t.Fatalf("create Alpha: %v", err)
	}
	if _, err := f.repo.Create(ctx, f.rcA, otherSphere.Canonical(), nil, "Gamma"); err != nil {
		t.Fatalf("create Gamma in other sphere: %v", err)
	}

	list, err := f.repo.List(ctx, f.rcA, sphereRID.Canonical())
	if err != nil {
		t.Fatalf("list facets: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	if list[0].Name != "Alpha" || list[1].Name != "Beta" {
		t.Errorf("list order = [%q, %q], want [Alpha, Beta]", list[0].Name, list[1].Name)
	}
}

// TestGetFacetCrossTenantNotAccessible confirms a Facet Record_ID from another
// tenant is not accessible (Req 1.6).
func TestGetFacetCrossTenantNotAccessible(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbB, sphereRID, "B Sphere")
	created, err := f.repo.Create(ctx, f.rcB, sphereRID.Canonical(), nil, "In B")
	if err != nil {
		t.Fatalf("create in B: %v", err)
	}

	if _, err := f.repo.Get(ctx, f.rcA, created.RecordID); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant get: got %v, want ErrNotAccessible", err)
	}
}

// TestDeleteFacetCascadesDescendantsAndPolygons is the core cascade guarantee
// (Req 8.6): deleting a Facet removes it, every descendant Facet, and every
// Polygon contained anywhere in that subtree — all in one transaction.
func TestDeleteFacetCascadesDescendantsAndPolygons(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")

	// Build root -> child -> grandchild, each with a contained polygon, plus a
	// child polygon nested under the grandchild's polygon.
	root, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "Root")
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), &root.RecordID, "Child")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	grandchild, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), &child.RecordID, "Grandchild")
	if err != nil {
		t.Fatalf("create grandchild: %v", err)
	}

	// Resolve surrogate ids to attach polygons directly.
	rootSurr := facetSurrogate(t, f.dbA, root.RecordID)
	childSurr := facetSurrogate(t, f.dbA, child.RecordID)
	grandSurr := facetSurrogate(t, f.dbA, grandchild.RecordID)

	seedFacetPolygon(t, f.dbA, sphereSurrogate, sql.NullInt64{Int64: rootSurr, Valid: true}, sql.NullInt64{})
	seedFacetPolygon(t, f.dbA, sphereSurrogate, sql.NullInt64{Int64: childSurr, Valid: true}, sql.NullInt64{})
	parentPoly := seedFacetPolygon(t, f.dbA, sphereSurrogate, sql.NullInt64{Int64: grandSurr, Valid: true}, sql.NullInt64{})
	// A child polygon nested under the grandchild's polygon (parent_polygon_id
	// cascade must remove it too).
	seedFacetPolygon(t, f.dbA, sphereSurrogate, sql.NullInt64{Int64: grandSurr, Valid: true}, sql.NullInt64{Int64: parentPoly, Valid: true})

	// An unrelated Facet + Polygon in the same Sphere must survive.
	sibling, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "Sibling")
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}
	siblingSurr := facetSurrogate(t, f.dbA, sibling.RecordID)
	seedFacetPolygon(t, f.dbA, sphereSurrogate, sql.NullInt64{Int64: siblingSurr, Valid: true}, sql.NullInt64{})

	if err := f.repo.Delete(ctx, f.rcA, root.RecordID); err != nil {
		t.Fatalf("delete root facet: %v", err)
	}

	// Only the sibling Facet remains.
	if n := countFacets(t, f.dbA); n != 1 {
		t.Errorf("facet count after delete = %d, want 1 (sibling)", n)
	}
	// Only the sibling's Polygon remains.
	if n := countPolygons(t, f.dbA); n != 1 {
		t.Errorf("polygon count after delete = %d, want 1 (sibling polygon)", n)
	}

	// The deleted subtree is gone.
	if _, err := f.repo.Get(ctx, f.rcA, root.RecordID); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("root still accessible after delete: %v", err)
	}
	if _, err := f.repo.Get(ctx, f.rcA, grandchild.RecordID); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("grandchild still accessible after delete: %v", err)
	}
}

// TestDeleteFacetCrossTenantNotAccessible confirms deleting a Facet that is not
// in the caller's tenant returns ErrNotAccessible and changes nothing.
func TestDeleteFacetCrossTenantNotAccessible(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbB, sphereRID, "B Sphere")
	created, err := f.repo.Create(ctx, f.rcB, sphereRID.Canonical(), nil, "In B")
	if err != nil {
		t.Fatalf("create in B: %v", err)
	}

	before := countFacets(t, f.dbB)
	if err := f.repo.Delete(ctx, f.rcA, created.RecordID); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant delete: got %v, want ErrNotAccessible", err)
	}
	if after := countFacets(t, f.dbB); after != before {
		t.Errorf("tenant B facet count changed by cross-tenant delete: before=%d after=%d", before, after)
	}
}

// facetSurrogate resolves a Facet Record_ID to its surrogate id directly from a
// tenant handle, for tests that need to attach child rows.
func facetSurrogate(t *testing.T, db *sql.DB, recordID string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM facet WHERE record_id = ?`, recordID).Scan(&id); err != nil {
		t.Fatalf("resolve facet surrogate: %v", err)
	}
	return id
}
