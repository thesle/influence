package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/influence/influence/internal/recordid"
)

// These tests exercise Sphere CRUD over the real production tenant-routing path
// (the twoTenantFixture from base_test.go): file-backed tenants behind a real
// ConnManager. They verify name validation and uniqueness (Req 6.4), Record_ID
// assignment (Req 6.3), tenant-scoped Get/List, and the cascade delete of a
// Sphere's Facets and Polygons in a single transaction (Req 6.5).

// seedFacet inserts a Facet row under the given sphere surrogate id and returns
// its surrogate id. Optional parent chains via parentID (0 = top-level).
func seedFacet(t *testing.T, db *sql.DB, sphereID int64, parentID int64, name string) int64 {
	t.Helper()
	rid := recordid.New()
	var parent any
	if parentID != 0 {
		parent = parentID
	}
	res, err := db.Exec(
		`INSERT INTO facet (record_id, sphere_id, parent_facet_id, name) VALUES (?, ?, ?, ?)`,
		rid.Canonical(), sphereID, parent, name,
	)
	if err != nil {
		t.Fatalf("seed facet: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("facet last insert id: %v", err)
	}
	return id
}

// seedPolygon inserts a Polygon row under the given sphere surrogate id.
func seedPolygon(t *testing.T, db *sql.DB, sphereID int64) int64 {
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
	return id
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestSphereCreateStoresAndAssignsRecordID confirms a valid name is stored in
// the caller's tenant with an assigned Record_ID (Req 6.2, 6.3).
func TestSphereCreateStoresAndAssignsRecordID(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	sph, err := repo.Create(ctx, f.rcA, "Engineering")
	if err != nil {
		t.Fatalf("create sphere: %v", err)
	}
	if sph.Name != "Engineering" {
		t.Errorf("name = %q, want %q", sph.Name, "Engineering")
	}
	if sph.RecordID.Canonical() == "" {
		t.Error("expected an assigned Record_ID")
	}

	// Stored in tenant A only.
	if got := countRows(t, f.dbA, "sphere"); got != 1 {
		t.Errorf("tenant A sphere count = %d, want 1", got)
	}
	if got := countRows(t, f.dbB, "sphere"); got != 0 {
		t.Errorf("tenant B sphere count = %d, want 0", got)
	}

	// Round-trips through Get.
	got, err := repo.Get(ctx, f.rcA, sph.RecordID)
	if err != nil {
		t.Fatalf("get sphere: %v", err)
	}
	if got.Name != "Engineering" || got.RecordID.Canonical() != sph.RecordID.Canonical() {
		t.Errorf("get = %+v, want name Engineering / id %s", got, sph.RecordID)
	}
}

// TestSphereCreateBoundaryLengths confirms names of length 1 and 100 are
// accepted and length 101 is rejected (Req 6.2, 6.4).
func TestSphereCreateBoundaryLengths(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	if _, err := repo.Create(ctx, f.rcA, "x"); err != nil {
		t.Errorf("1-char name should be accepted, got %v", err)
	}
	if _, err := repo.Create(ctx, f.rcA, strings.Repeat("a", 100)); err != nil {
		t.Errorf("100-char name should be accepted, got %v", err)
	}
	if _, err := repo.Create(ctx, f.rcA, strings.Repeat("a", 101)); !errors.Is(err, ErrValidation) {
		t.Errorf("101-char name: got %v, want ErrValidation", err)
	}
}

// TestSphereCreateEmptyRejected confirms an empty name is rejected with
// ErrValidation and leaves the Sphere set unchanged (Req 6.4).
func TestSphereCreateEmptyRejected(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	before := countRows(t, f.dbA, "sphere")
	if _, err := repo.Create(ctx, f.rcA, ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty name: got %v, want ErrValidation", err)
	}
	if after := countRows(t, f.dbA, "sphere"); after != before {
		t.Errorf("empty-name rejection changed sphere count: before=%d after=%d", before, after)
	}
}

// TestSphereCreateOversizeLeavesSetUnchanged confirms an over-length name is
// rejected and no row is written (Req 6.4).
func TestSphereCreateOversizeLeavesSetUnchanged(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	if _, err := repo.Create(ctx, f.rcA, "Keeper"); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	before := countRows(t, f.dbA, "sphere")

	if _, err := repo.Create(ctx, f.rcA, strings.Repeat("z", 500)); !errors.Is(err, ErrValidation) {
		t.Fatalf("oversize name: got %v, want ErrValidation", err)
	}
	if after := countRows(t, f.dbA, "sphere"); after != before {
		t.Errorf("oversize rejection changed sphere count: before=%d after=%d", before, after)
	}
}

// TestSphereCreateDuplicateRejected confirms a duplicate name within the tenant
// is rejected and leaves the existing Sphere set unchanged (Req 6.4).
func TestSphereCreateDuplicateRejected(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	if _, err := repo.Create(ctx, f.rcA, "Ops"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	before := countRows(t, f.dbA, "sphere")

	if _, err := repo.Create(ctx, f.rcA, "Ops"); !errors.Is(err, ErrValidation) {
		t.Fatalf("duplicate name: got %v, want ErrValidation", err)
	}
	if after := countRows(t, f.dbA, "sphere"); after != before {
		t.Errorf("duplicate rejection changed sphere count: before=%d after=%d", before, after)
	}
}

// TestSphereDuplicateNameAllowedAcrossTenants confirms uniqueness is scoped to a
// single tenant: the same name may exist in two different tenants.
func TestSphereDuplicateNameAllowedAcrossTenants(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	if _, err := repo.Create(ctx, f.rcA, "Shared"); err != nil {
		t.Fatalf("create in A: %v", err)
	}
	if _, err := repo.Create(ctx, f.rcB, "Shared"); err != nil {
		t.Fatalf("same name in B should be allowed: %v", err)
	}
}

// TestSphereGetCrossTenantNotAccessible confirms a Sphere owned by tenant B is
// not accessible from tenant A (Req 1.6).
func TestSphereGetCrossTenantNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	sph, err := repo.Create(ctx, f.rcB, "OnlyB")
	if err != nil {
		t.Fatalf("create in B: %v", err)
	}
	if _, err := repo.Get(ctx, f.rcA, sph.RecordID); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant get: got %v, want ErrNotAccessible", err)
	}
}

// TestSphereListOrderedByNameCaseInsensitive confirms List returns the tenant's
// Spheres ordered case-insensitively by name.
func TestSphereListOrderedByNameCaseInsensitive(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	for _, name := range []string{"banana", "Apple", "cherry"} {
		if _, err := repo.Create(ctx, f.rcA, name); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}
	list, err := repo.List(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"Apple", "banana", "cherry"}
	if len(list) != len(want) {
		t.Fatalf("list len = %d, want %d", len(list), len(want))
	}
	for i, s := range list {
		if s.Name != want[i] {
			t.Errorf("list[%d] = %q, want %q", i, s.Name, want[i])
		}
	}
}

// TestSphereDeleteCascades confirms deleting a Sphere removes all its Facets and
// Polygons in a single transaction (Req 6.5), while an unrelated Sphere in the
// same tenant is untouched.
func TestSphereDeleteCascades(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	// Target sphere with a facet hierarchy and polygons.
	target, err := repo.Create(ctx, f.rcA, "Target")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	var targetSurrogate int64
	if err := f.dbA.QueryRow(`SELECT id FROM sphere WHERE record_id = ?`, target.RecordID.Canonical()).Scan(&targetSurrogate); err != nil {
		t.Fatalf("resolve target surrogate: %v", err)
	}
	parentFacet := seedFacet(t, f.dbA, targetSurrogate, 0, "Parent")
	seedFacet(t, f.dbA, targetSurrogate, parentFacet, "Child")
	seedPolygon(t, f.dbA, targetSurrogate)
	seedPolygon(t, f.dbA, targetSurrogate)

	// An unrelated sphere with its own facet/polygon that must survive.
	keeper, err := repo.Create(ctx, f.rcA, "Keeper")
	if err != nil {
		t.Fatalf("create keeper: %v", err)
	}
	var keeperSurrogate int64
	if err := f.dbA.QueryRow(`SELECT id FROM sphere WHERE record_id = ?`, keeper.RecordID.Canonical()).Scan(&keeperSurrogate); err != nil {
		t.Fatalf("resolve keeper surrogate: %v", err)
	}
	seedFacet(t, f.dbA, keeperSurrogate, 0, "KeeperFacet")
	seedPolygon(t, f.dbA, keeperSurrogate)

	if err := repo.Delete(ctx, f.rcA, target.RecordID); err != nil {
		t.Fatalf("delete target: %v", err)
	}

	// Target and its descendants are gone; keeper's remain.
	if got := countRows(t, f.dbA, "sphere"); got != 1 {
		t.Errorf("sphere count = %d, want 1 (keeper only)", got)
	}
	if got := countRows(t, f.dbA, "facet"); got != 1 {
		t.Errorf("facet count = %d, want 1 (keeper's facet only)", got)
	}
	if got := countRows(t, f.dbA, "polygon"); got != 1 {
		t.Errorf("polygon count = %d, want 1 (keeper's polygon only)", got)
	}

	// The keeper is still retrievable.
	if _, err := repo.Get(ctx, f.rcA, keeper.RecordID); err != nil {
		t.Errorf("keeper should survive cascade: %v", err)
	}
}

// TestSphereDeleteMissingNotAccessible confirms deleting an absent Record_ID
// yields ErrNotAccessible and changes nothing (Req 1.6).
func TestSphereDeleteMissingNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	if _, err := repo.Create(ctx, f.rcA, "Present"); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	before := countRows(t, f.dbA, "sphere")

	if err := repo.Delete(ctx, f.rcA, recordid.New()); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("delete missing: got %v, want ErrNotAccessible", err)
	}
	if after := countRows(t, f.dbA, "sphere"); after != before {
		t.Errorf("failed delete changed sphere count: before=%d after=%d", before, after)
	}
}

// TestSphereDeleteCrossTenantNotAccessible confirms tenant A cannot delete a
// Sphere owned by tenant B, and B's data is unchanged (Req 1.6).
func TestSphereDeleteCrossTenantNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &SphereRepo{Base: f.base}
	ctx := context.Background()

	sph, err := repo.Create(ctx, f.rcB, "OnlyB")
	if err != nil {
		t.Fatalf("create in B: %v", err)
	}
	before := countRows(t, f.dbB, "sphere")

	if err := repo.Delete(ctx, f.rcA, sph.RecordID); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant delete: got %v, want ErrNotAccessible", err)
	}
	if after := countRows(t, f.dbB, "sphere"); after != before {
		t.Errorf("cross-tenant delete changed tenant B: before=%d after=%d", before, after)
	}
}
