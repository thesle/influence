package repo

import (
	"context"
	"errors"
	"testing"

	"github.com/influence/influence/internal/recordid"
)

// These tests exercise Polygon edit-mode resolution (Req 25.1): every Polygon
// operates in exactly one of two mutually exclusive modes recorded in
// POLYGON.edit_mode. They run over the production tenant-routing fixture so
// resolution is tenant-scoped like every other Polygon operation.

// setEditMode writes edit_mode directly onto a Polygon row in a tenant DB. It
// exists so tests can place a Polygon into collaborative mode without a
// dedicated setter (Create defaults to record-locking per the schema).
func setEditMode(t *testing.T, f polygonFixture, polygonRID, mode string) {
	t.Helper()
	if _, err := f.dbA.Exec(`UPDATE polygon SET edit_mode = ? WHERE record_id = ?`, mode, polygonRID); err != nil {
		t.Fatalf("set edit_mode: %v", err)
	}
}

// TestPolygonCreateDefaultsToLocking confirms a freshly created Polygon is in
// record-locking mode, the schema default (Req 25.1), both in the returned
// value and after a round-trip through Get.
func TestPolygonCreateDefaultsToLocking(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	created, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.EditMode != EditModeLocking {
		t.Errorf("created edit mode = %q, want %q", created.EditMode, EditModeLocking)
	}

	got, err := f.repo.Get(ctx, f.rcA, created.RecordID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.EditMode != EditModeLocking {
		t.Errorf("got edit mode = %q, want %q", got.EditMode, EditModeLocking)
	}
}

// TestPolygonModeReturnsPerPolygonMode confirms Mode reports each Polygon's own
// single mode: one left at the locking default, another switched to
// collaborative (Req 25.1).
func TestPolygonModeReturnsPerPolygonMode(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	locking, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{})
	if err != nil {
		t.Fatalf("create locking polygon: %v", err)
	}
	collab, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{})
	if err != nil {
		t.Fatalf("create collaborative polygon: %v", err)
	}
	setEditMode(t, f, collab.RecordID, string(EditModeCollaborative))

	gotLock, err := f.repo.Mode(ctx, f.rcA, locking.RecordID)
	if err != nil {
		t.Fatalf("mode locking: %v", err)
	}
	if gotLock != EditModeLocking {
		t.Errorf("locking polygon mode = %q, want %q", gotLock, EditModeLocking)
	}

	gotCollab, err := f.repo.Mode(ctx, f.rcA, collab.RecordID)
	if err != nil {
		t.Fatalf("mode collaborative: %v", err)
	}
	if gotCollab != EditModeCollaborative {
		t.Errorf("collaborative polygon mode = %q, want %q", gotCollab, EditModeCollaborative)
	}
}

// TestPolygonModeCrossTenantNotAccessible confirms Mode is tenant-scoped: a
// Polygon that exists only in another tenant is not resolvable (Req 1.6).
func TestPolygonModeCrossTenantNotAccessible(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	// A Polygon created in tenant B.
	sphereRID := recordid.New()
	seedSphere(t, f.dbB, sphereRID, "Only In B")
	created, err := f.repo.Create(ctx, f.rcB, sphereRID.Canonical(), PolygonMarkdown, 10, CreateOptions{})
	if err != nil {
		t.Fatalf("create in B: %v", err)
	}

	// Tenant A cannot resolve its mode.
	_, err = f.repo.Mode(ctx, f.rcA, created.RecordID)
	if !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant mode: got %v, want ErrNotAccessible", err)
	}

	// Tenant B, the owner, resolves it.
	if _, err := f.repo.Mode(ctx, f.rcB, created.RecordID); err != nil {
		t.Fatalf("owner mode: %v", err)
	}
}

// TestPolygonModeMissingNotAccessible confirms an unparseable or absent
// Record_ID yields ErrNotAccessible rather than a raw error (Req 1.6).
func TestPolygonModeMissingNotAccessible(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	if _, err := f.repo.Mode(ctx, f.rcA, "not-a-record-id"); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("unparseable id: got %v, want ErrNotAccessible", err)
	}
	if _, err := f.repo.Mode(ctx, f.rcA, recordid.New().Canonical()); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("absent id: got %v, want ErrNotAccessible", err)
	}
}

// TestEditModeValid confirms only the two supported modes are valid (Req 25.1).
func TestEditModeValid(t *testing.T) {
	for _, m := range []EditMode{EditModeLocking, EditModeCollaborative} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	for _, m := range []EditMode{EditMode(""), EditMode("bogus")} {
		if m.Valid() {
			t.Errorf("%q should be invalid", m)
		}
	}
}
