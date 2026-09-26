package repo

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/influence/influence/internal/recordid"
)

// parentRecordID reads the parent Facet's Record_ID for the given Facet directly
// from a tenant handle, or "" when the Facet is top-level. Tests use it to
// assert the hierarchy is (or is not) changed independent of the repository.
func parentRecordID(t *testing.T, db *sql.DB, recordID string) string {
	t.Helper()
	var parent sql.NullString
	err := db.QueryRow(
		`SELECT p.record_id
		 FROM facet f
		 LEFT JOIN facet p ON p.id = f.parent_facet_id
		 WHERE f.record_id = ?`, recordID).Scan(&parent)
	if err != nil {
		t.Fatalf("read parent record id: %v", err)
	}
	if parent.Valid {
		return parent.String
	}
	return ""
}

// TestReparentMovesFacetUnderNewParent confirms a valid reparent updates the
// Facet's parent within the same Sphere (Req 9.2).
func TestReparentMovesFacetUnderNewParent(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	a, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "A")
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "B")
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if err := f.repo.Reparent(ctx, f.rcA, b.RecordID, &a.RecordID); err != nil {
		t.Fatalf("reparent B under A: %v", err)
	}
	if got := parentRecordID(t, f.dbA, b.RecordID); got != a.RecordID {
		t.Errorf("B parent = %q, want %q", got, a.RecordID)
	}
}

// TestReparentToTopLevelClearsParent confirms a nil new parent detaches the
// Facet to the top level.
func TestReparentToTopLevelClearsParent(t *testing.T) {
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

	if err := f.repo.Reparent(ctx, f.rcA, child.RecordID, nil); err != nil {
		t.Fatalf("reparent child to top level: %v", err)
	}
	if got := parentRecordID(t, f.dbA, child.RecordID); got != "" {
		t.Errorf("child parent = %q, want top-level (empty)", got)
	}
}

// TestReparentRejectsSelfParent confirms setting a Facet's parent to itself is
// rejected with ErrHierarchy and leaves the hierarchy unchanged (Req 9.1).
func TestReparentRejectsSelfParent(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	a, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), nil, "A")
	if err != nil {
		t.Fatalf("create A: %v", err)
	}

	before := parentRecordID(t, f.dbA, a.RecordID)
	if err := f.repo.Reparent(ctx, f.rcA, a.RecordID, &a.RecordID); !errors.Is(err, ErrHierarchy) {
		t.Fatalf("self parent: got %v, want ErrHierarchy", err)
	}
	if after := parentRecordID(t, f.dbA, a.RecordID); after != before {
		t.Errorf("parent changed by rejected self-reparent: before=%q after=%q", before, after)
	}
}

// TestReparentRejectsDescendantParent confirms setting a Facet's parent to one
// of its own descendants is rejected with ErrHierarchy and changes nothing
// (Req 9.1). It uses a 3-level chain so the descendant is not a direct child.
func TestReparentRejectsDescendantParent(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

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

	before := parentRecordID(t, f.dbA, root.RecordID)
	// Moving root under its grandchild would create a cycle.
	if err := f.repo.Reparent(ctx, f.rcA, root.RecordID, &grandchild.RecordID); !errors.Is(err, ErrHierarchy) {
		t.Fatalf("descendant parent: got %v, want ErrHierarchy", err)
	}
	if after := parentRecordID(t, f.dbA, root.RecordID); after != before {
		t.Errorf("root parent changed by rejected reparent: before=%q after=%q", before, after)
	}
	// The intermediate links are also untouched.
	if got := parentRecordID(t, f.dbA, child.RecordID); got != root.RecordID {
		t.Errorf("child parent = %q, want %q", got, root.RecordID)
	}
	if got := parentRecordID(t, f.dbA, grandchild.RecordID); got != child.RecordID {
		t.Errorf("grandchild parent = %q, want %q", got, child.RecordID)
	}
}

// TestReparentRejectsCrossSphereParent confirms setting a Facet's parent to a
// Facet in a different Sphere is rejected with ErrHierarchy and changes nothing
// (Req 9.3).
func TestReparentRejectsCrossSphereParent(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereA := recordid.New()
	sphereB := recordid.New()
	seedSphere(t, f.dbA, sphereA, "Sphere A")
	seedSphere(t, f.dbA, sphereB, "Sphere B")

	facetInA, err := f.repo.Create(ctx, f.rcA, sphereA.Canonical(), nil, "In A")
	if err != nil {
		t.Fatalf("create facet in A: %v", err)
	}
	parentInB, err := f.repo.Create(ctx, f.rcA, sphereB.Canonical(), nil, "In B")
	if err != nil {
		t.Fatalf("create parent in B: %v", err)
	}

	before := parentRecordID(t, f.dbA, facetInA.RecordID)
	if err := f.repo.Reparent(ctx, f.rcA, facetInA.RecordID, &parentInB.RecordID); !errors.Is(err, ErrHierarchy) {
		t.Fatalf("cross-sphere parent: got %v, want ErrHierarchy", err)
	}
	if after := parentRecordID(t, f.dbA, facetInA.RecordID); after != before {
		t.Errorf("parent changed by rejected cross-sphere reparent: before=%q after=%q", before, after)
	}
}

// TestReparentCrossTenantNotAccessible confirms reparenting a Facet that is not
// in the caller's tenant returns ErrNotAccessible (Req 1.6).
func TestReparentCrossTenantNotAccessible(t *testing.T) {
	f := newFacetFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbB, sphereRID, "B Sphere")
	created, err := f.repo.Create(ctx, f.rcB, sphereRID.Canonical(), nil, "In B")
	if err != nil {
		t.Fatalf("create in B: %v", err)
	}

	if err := f.repo.Reparent(ctx, f.rcA, created.RecordID, nil); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant reparent: got %v, want ErrNotAccessible", err)
	}
}
