package service

// Unit tests for PDF export (Requirements 22.1, 22.2, 22.3).
//
// These tests inject a MOCK HTMLToPDF renderer so the export logic — reusing the
// app's render + Masker path, inheriting masking, and failing without producing
// a file — is verified without a headless-browser dependency. The mock captures
// the exact HTML the exporter would convert, which is what lets us assert the
// print-only layout omits toolbars/menus (22.1) and that a no-reveal user's HTML
// carries only masked placeholders (22.2). A failing mock exercises the
// no-file-on-failure path (22.3).

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/influence/influence/internal/data"
)

// mockRenderer is a test HTMLToPDF. It records the last HTML passed to Convert
// and returns either a fixed PDF payload or a configured error, so tests can
// assert on the HTML that would be rendered and on failure handling.
type mockRenderer struct {
	lastHTML []byte
	pdf      []byte
	err      error
	calls    int
}

func (m *mockRenderer) Convert(_ context.Context, htmlDoc []byte) ([]byte, error) {
	m.calls++
	m.lastHTML = htmlDoc
	if m.err != nil {
		return nil, m.err
	}
	return m.pdf, nil
}

// pdfFixture stands up a real, file-backed tenant behind a real ConnManager
// (same path the encryption/linkresolver fixtures use) so PDFExport runs over
// the production tenant-routing + PolicyEngine + render path, with only the
// HTML→PDF conversion mocked.
type pdfFixture struct {
	exporter *PDFExporter
	renderer *mockRenderer
	rc       data.RequestContext
	db       *sql.DB
	conns    data.ConnManager
	policy   PolicyEngine
	links    *LinkResolver
}

func newPDFFixture(t *testing.T, renderer *mockRenderer) pdfFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	central, err := sql.Open(data.DriverName, filepath.Join(dir, "central.db"))
	if err != nil {
		t.Fatalf("open central: %v", err)
	}
	if err := data.MigrateCentral(ctx, central); err != nil {
		t.Fatalf("migrate central: %v", err)
	}

	pathA := filepath.Join(dir, "tenantA.db")
	insertEncTenant(t, central, 1, "tenant-a-uuid", "A", pathA)
	dbA := migrateEncTenantFile(t, pathA)

	conns := data.NewConnManager(central)
	t.Cleanup(func() { _ = conns.Close() })

	policy := NewPolicyEngine()
	links := NewLinkResolver(conns, policy)

	return pdfFixture{
		exporter: NewPDFExporter(conns, policy, links, renderer),
		renderer: renderer,
		rc:       data.NewRequestContext(10, 1, nil, nil),
		db:       dbA,
		conns:    conns,
		policy:   policy,
		links:    links,
	}
}

// --- 22.1: print-only HTML reuses the render path and omits chrome ----------

func TestPDFExportProducesPrintOnlyHTML(t *testing.T) {
	renderer := &mockRenderer{pdf: []byte("%PDF-1.4 fake")}
	f := newPDFFixture(t, renderer)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S1")
	poly := seedPolygonRow(t, f.db, sphere, "# Title\n\nHello **world**")
	authz := grantAccess(sphere, data.AccessRead)

	pdf, err := f.exporter.PDFExport(ctx, f.rc, authz, poly)
	if err != nil {
		t.Fatalf("PDFExport: %v", err)
	}
	if string(pdf) != "%PDF-1.4 fake" {
		t.Fatalf("expected mock PDF bytes, got %q", pdf)
	}
	if renderer.calls != 1 {
		t.Fatalf("expected renderer called once, got %d", renderer.calls)
	}

	got := string(renderer.lastHTML)

	// Content rendered through the same markdown render path.
	if !strings.Contains(got, "<h1>Title</h1>") {
		t.Errorf("rendered HTML missing heading; got:\n%s", got)
	}
	if !strings.Contains(got, "<strong>world</strong>") {
		t.Errorf("rendered HTML missing rendered markdown emphasis; got:\n%s", got)
	}

	// Print-only layout: a self-contained document with print styling and no
	// editor/navigation chrome (Req 22.1).
	if !strings.Contains(got, "<!DOCTYPE html>") {
		t.Errorf("expected a full print document; got:\n%s", got)
	}
	if !strings.Contains(got, "@page") {
		t.Errorf("expected print-oriented @page styling; got:\n%s", got)
	}
	for _, chrome := range []string{"<nav", "toolbar", "menu", "<button"} {
		if strings.Contains(strings.ToLower(got), chrome) {
			t.Errorf("print HTML must omit chrome, but contained %q; got:\n%s", chrome, got)
		}
	}
}

// --- 22.2: masking is inherited — no-reveal user gets placeholders ----------

func TestPDFExportMasksForNoRevealUser(t *testing.T) {
	renderer := &mockRenderer{pdf: []byte("%PDF")}
	f := newPDFFixture(t, renderer)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "Secret Sphere")
	// Stored content holds an Obfuscation_Token, never plaintext. "SECRET42"
	// stands in for the token id / would-be plaintext that must not appear.
	poly := seedPolygonRow(t, f.db, sphere, "# Doc\n\nBefore |encrypt|SECRET42| after")

	// Read access but NO reveal grant for this Sphere.
	authz := grantAccess(sphere, data.AccessRead)

	if _, err := f.exporter.PDFExport(ctx, f.rc, authz, poly); err != nil {
		t.Fatalf("PDFExport: %v", err)
	}

	got := string(renderer.lastHTML)
	if !strings.Contains(got, MaskedPlaceholder) {
		t.Errorf("expected masked placeholder %q in HTML; got:\n%s", MaskedPlaceholder, got)
	}
	// Neither the token id/plaintext nor the raw ciphertext token must appear.
	if strings.Contains(got, "SECRET42") {
		t.Errorf("no-reveal PDF HTML leaked token contents; got:\n%s", got)
	}
	if strings.Contains(got, "|encrypt|") {
		t.Errorf("no-reveal PDF HTML leaked raw Obfuscation_Token; got:\n%s", got)
	}
}

// A reveal-permitted context still routes through the Masker with reveal=true;
// with the default (nil) revealer, tokens stay masked rather than leaking, which
// confirms the exporter never bypasses the seam.
func TestPDFExportRevealContextStillRoutesThroughMasker(t *testing.T) {
	renderer := &mockRenderer{pdf: []byte("%PDF")}
	f := newPDFFixture(t, renderer)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S")
	poly := seedPolygonRow(t, f.db, sphere, "# Doc\n\n|encrypt|TOKENID|")
	authz := AuthzData{Grants: []GroupGrant{{Group: 1, Sphere: sphere, Access: data.AccessRead, Reveal: true}}}

	if _, err := f.exporter.PDFExport(ctx, f.rc, authz, poly); err != nil {
		t.Fatalf("PDFExport: %v", err)
	}
	got := string(renderer.lastHTML)
	if strings.Contains(got, "|encrypt|") {
		t.Errorf("raw token leaked into HTML; got:\n%s", got)
	}
}

// --- 22.3: on failure produce no file and return an error -------------------

func TestPDFExportRendererFailureProducesNoFile(t *testing.T) {
	boom := errors.New("headless renderer crashed")
	renderer := &mockRenderer{err: boom}
	f := newPDFFixture(t, renderer)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S")
	poly := seedPolygonRow(t, f.db, sphere, "# Doc\n\nbody")
	authz := grantAccess(sphere, data.AccessRead)

	pdf, err := f.exporter.PDFExport(ctx, f.rc, authz, poly)
	if err == nil {
		t.Fatalf("expected an error on renderer failure")
	}
	if !errors.Is(err, ErrPDFGenerationFailed) {
		t.Errorf("expected ErrPDFGenerationFailed, got %v", err)
	}
	if pdf != nil {
		t.Errorf("expected no PDF file on failure, got %d bytes", len(pdf))
	}
}

// A renderer that returns no error but no bytes must also be treated as a
// failure — an empty file is not a produced file (Req 22.3).
func TestPDFExportEmptyOutputIsFailure(t *testing.T) {
	renderer := &mockRenderer{pdf: nil}
	f := newPDFFixture(t, renderer)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S")
	poly := seedPolygonRow(t, f.db, sphere, "# Doc\n\nbody")
	authz := grantAccess(sphere, data.AccessRead)

	pdf, err := f.exporter.PDFExport(ctx, f.rc, authz, poly)
	if !errors.Is(err, ErrPDFGenerationFailed) {
		t.Errorf("expected ErrPDFGenerationFailed for empty output, got %v", err)
	}
	if pdf != nil {
		t.Errorf("expected no PDF on empty output, got %d bytes", len(pdf))
	}
}

// An inaccessible Polygon (no grant) yields not-accessible and never calls the
// renderer, so nothing is produced.
func TestPDFExportNotAccessibleProducesNoFile(t *testing.T) {
	renderer := &mockRenderer{pdf: []byte("%PDF")}
	f := newPDFFixture(t, renderer)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S")
	poly := seedPolygonRow(t, f.db, sphere, "# Doc\n\nbody")
	// No grant on the Sphere: caller cannot access it.
	authz := AuthzData{}

	pdf, err := f.exporter.PDFExport(ctx, f.rc, authz, poly)
	if !errors.Is(err, ErrPolygonNotAccessible) {
		t.Errorf("expected ErrPolygonNotAccessible, got %v", err)
	}
	if pdf != nil {
		t.Errorf("expected no PDF for inaccessible polygon, got %d bytes", len(pdf))
	}
	if renderer.calls != 0 {
		t.Errorf("renderer must not be called for inaccessible polygon, calls=%d", renderer.calls)
	}
}

// The default (nil) renderer fails closed so an unconfigured deployment produces
// no file rather than requiring a browser dependency.
func TestPDFExportUnconfiguredRendererFailsClosed(t *testing.T) {
	// Build a fixture, then rebuild the exporter with an untyped-nil HTMLToPDF
	// so NewPDFExporter installs its fail-closed default renderer.
	f := newPDFFixture(t, &mockRenderer{})
	f.exporter = NewPDFExporter(f.conns, f.policy, f.links, nil)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S")
	poly := seedPolygonRow(t, f.db, sphere, "# Doc\n\nbody")
	authz := grantAccess(sphere, data.AccessRead)

	pdf, err := f.exporter.PDFExport(ctx, f.rc, authz, poly)
	if err == nil {
		t.Fatalf("expected an error from the unconfigured renderer")
	}
	if !errors.Is(err, ErrPDFGenerationFailed) {
		t.Errorf("expected ErrPDFGenerationFailed wrapping unavailability, got %v", err)
	}
	if pdf != nil {
		t.Errorf("expected no PDF from unconfigured renderer, got %d bytes", len(pdf))
	}
}
