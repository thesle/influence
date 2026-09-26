package service

// End-to-end integration test for PDF export (Requirements 22.1, 22.2).
//
// The unit tests in pdf_test.go inject a mock renderer and verify the export
// logic in isolation: the print-only layout omits chrome (22.1) and a no-reveal
// user's HTML carries only placeholders when the stored content already holds a
// literal Obfuscation_Token (22.2). Those seed a token by hand.
//
// This test exercises the FULL production stack for that same guarantee: it
// runs the real EncryptionService.EncryptSpan against a real, key-provisioned
// tenant so a genuine ciphertext row is written and a real token id is minted,
// then drives the complete PDFExport path (tenant routing → PolicyEngine
// authorization → Cross-Refraction resolution → the single Masker seam →
// markdown render → print-only wrap) with a mock renderer standing in only for
// the headless browser. That proves masking parity is structural: the PDF a
// no-reveal user gets shows the masked placeholder and NEVER the plaintext, the
// minted token marker, or the stored ciphertext — for content encrypted through
// the app's own encrypt flow, not a hand-written token (Req 22.2) — while the
// converted document still omits every toolbar and menu (Req 22.1).

import (
	"context"
	"strings"
	"testing"

	"github.com/influence/influence/internal/data"
)

// TestPDFExportEndToEndMasksRealEncryptedSpan encrypts a real span through the
// EncryptionService, then exports the Polygon as a user with read but no reveal
// access and asserts, through the full PDFExport path, that the generated PDF
// content masks the real encrypted span and omits chrome.
//
// Validates: Requirements 22.1, 22.2
func TestPDFExportEndToEndMasksRealEncryptedSpan(t *testing.T) {
	renderer := &mockRenderer{pdf: []byte("%PDF-1.4 fake")}
	f := newPDFFixture(t, renderer)
	ctx := context.Background()

	// Provision key material for the tenant so the real encrypt flow can unwrap
	// the DEK, then build the production EncryptionService over the SAME conns.
	if err := InitTenantSecret(ctx, f.db, encTestMaster); err != nil {
		t.Fatalf("init tenant secret: %v", err)
	}
	enc := NewEncryptionService(f.conns, encTestMaster)

	// Seed a Polygon whose content holds a clearly-identifiable secret plaintext.
	const secret = "TOPSECRET-PLAINTEXT"
	sphere := seedSphereRow(t, f.db, "Confidential Sphere")
	content := "# Report\n\nBefore " + secret + " after"
	poly := seedPolygonRow(t, f.db, sphere, content)

	// Encrypt the secret span for real: this writes a ciphertext row and
	// rewrites POLYGON.content to hold the minted Obfuscation_Token in its place.
	start := strings.Index(content, secret)
	res, err := enc.EncryptSpan(ctx, f.rc, poly, start, start+len(secret))
	if err != nil {
		t.Fatalf("EncryptSpan: %v", err)
	}
	if res.TokenID == "" {
		t.Fatal("expected a minted token id from EncryptSpan")
	}
	// Sanity: the stored content now holds the real token, not the plaintext.
	if strings.Contains(res.NewContent, secret) {
		t.Fatalf("precondition failed: stored content still holds plaintext: %q", res.NewContent)
	}
	tokenMarker := "|encrypt|" + res.TokenID + "|"
	if !strings.Contains(res.NewContent, tokenMarker) {
		t.Fatalf("precondition failed: stored content missing token %q: %q", tokenMarker, res.NewContent)
	}

	// Export as a user with read access but NO reveal grant for the Sphere.
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

	// The HTML handed to the renderer represents the PDF's content.
	got := string(renderer.lastHTML)

	// --- Req 22.2: end-to-end masking parity for REAL encrypted content ------
	if !strings.Contains(got, MaskedPlaceholder) {
		t.Errorf("expected masked placeholder %q in PDF content; got:\n%s", MaskedPlaceholder, got)
	}
	// The original plaintext must not appear anywhere in the exported content.
	if strings.Contains(got, secret) {
		t.Errorf("PDF content leaked encrypted plaintext %q; got:\n%s", secret, got)
	}
	// Neither the minted token marker nor the bare token id may leak.
	if strings.Contains(got, tokenMarker) {
		t.Errorf("PDF content leaked raw Obfuscation_Token %q; got:\n%s", tokenMarker, got)
	}
	if strings.Contains(got, res.TokenID) {
		t.Errorf("PDF content leaked minted token id %q; got:\n%s", res.TokenID, got)
	}

	// Read the real ciphertext straight from the tenant DB and prove it is not
	// present in the exported content either (no ciphertext egress, Req 22.2).
	var ciphertext []byte
	if err := f.db.QueryRow(
		`SELECT ciphertext FROM ciphertext WHERE token_id = ?`, res.TokenID,
	).Scan(&ciphertext); err != nil {
		t.Fatalf("read ciphertext row: %v", err)
	}
	if len(ciphertext) == 0 {
		t.Fatal("expected a non-empty ciphertext row from the real encrypt flow")
	}
	if strings.Contains(got, string(ciphertext)) {
		t.Errorf("PDF content leaked raw ciphertext bytes; got:\n%s", got)
	}

	// --- Req 22.1: the same end-to-end document still omits all chrome -------
	if !strings.Contains(got, "<!DOCTYPE html>") {
		t.Errorf("expected a full print document; got:\n%s", got)
	}
	if !strings.Contains(got, "@page") {
		t.Errorf("expected print-oriented @page styling; got:\n%s", got)
	}
	if !strings.Contains(got, "<h1>Report</h1>") {
		t.Errorf("expected the rendered Polygon heading; got:\n%s", got)
	}
	for _, chrome := range []string{"<nav", "toolbar", "menu", "<button"} {
		if strings.Contains(strings.ToLower(got), chrome) {
			t.Errorf("PDF content must omit chrome, but contained %q; got:\n%s", chrome, got)
		}
	}
}
