package repo

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/influence/influence/internal/recordid"
)

// These tests exercise the Polygon search-index component over the real
// production tenant-routing path (the twoTenantFixture from base_test.go):
// file-backed tenants behind a real ConnManager, with a real polygon_fts FTS5
// virtual table created by MigrateTenant.
//
// They verify the non-leakage guarantee (Req 16.7, 23.2) — the indexed row
// holds only the masked placeholder, never the encrypted span's plaintext or
// ciphertext — and that the index stays in sync when a Polygon is created,
// edited, and deleted.

// indexTestPlaceholder is the placeholder the test masker substitutes for an
// Obfuscation_Token. It intentionally mirrors the shape of service.MaskedPlaceholder
// (a fixed marker containing neither plaintext nor ciphertext) without importing
// the service package, keeping this repo test dependency-free.
const indexTestPlaceholder = "[[hidden]]"

// tokenPattern matches a stored Obfuscation_Token `|encrypt|{id}|`, mirroring
// the canonical token format the real masker recognizes (Req 16.2).
var tokenPattern = regexp.MustCompile(`\|encrypt\|[^|]+\|`)

// testMask is a stand-in for service.Mask(content, false, nil): it replaces
// every Obfuscation_Token with the placeholder so neither plaintext nor
// ciphertext survives into the index.
func testMask(content string) string {
	return tokenPattern.ReplaceAllString(content, indexTestPlaceholder)
}

// indexFixture pairs the two-tenant fixture with a PolygonIndex bound to the
// same ConnManager and the test masker.
type indexFixture struct {
	twoTenantFixture
	index *PolygonIndex
}

func newIndexFixture(t *testing.T) indexFixture {
	t.Helper()
	f := newTwoTenantFixture(t)
	return indexFixture{
		twoTenantFixture: f,
		index:            &PolygonIndex{Base: f.base, mask: testMask},
	}
}

// ftsContent reads the raw indexed content stored for a Polygon record id, and
// whether any row exists for it.
func ftsContent(t *testing.T, f indexFixture, recordID string) (string, bool) {
	t.Helper()
	var content string
	err := f.dbA.QueryRow(
		`SELECT content FROM polygon_fts WHERE record_id = ?`, recordID,
	).Scan(&content)
	if err != nil {
		return "", false
	}
	return content, true
}

// ftsMatch returns the record ids in tenant A whose indexed content matches the
// FTS query term.
func ftsMatch(t *testing.T, f indexFixture, term string) []string {
	t.Helper()
	rows, err := f.dbA.Query(
		`SELECT record_id FROM polygon_fts WHERE polygon_fts MATCH ?`, term,
	)
	if err != nil {
		t.Fatalf("fts match %q: %v", term, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err != nil {
			t.Fatalf("scan match: %v", err)
		}
		out = append(out, rid)
	}
	return out
}

// TestIndexPolygonExcludesPlaintextAndCiphertext is the core non-leakage
// guarantee: content with an Obfuscation_Token is indexed as its masked
// placeholder, and neither the plaintext nor the ciphertext of the encrypted
// span appears in the index (Req 16.7, 23.2).
func TestIndexPolygonExcludesPlaintextAndCiphertext(t *testing.T) {
	f := newIndexFixture(t)
	ctx := context.Background()

	rid := recordid.New().Canonical()
	// Stored content: a token stands in for the secret; the plaintext lives
	// nowhere in stored content, and the ciphertext (simulated here) is never
	// part of the content at all.
	const plaintext = "topsecretvalue"
	const ciphertext = "AAECAwQFBgcICQoLDA0ODw"
	stored := "alpha bravo |encrypt|tok-123| charlie"

	if err := f.index.IndexPolygon(ctx, f.rcA, rid, stored); err != nil {
		t.Fatalf("index polygon: %v", err)
	}

	indexed, ok := ftsContent(t, f, rid)
	if !ok {
		t.Fatalf("no index row for %q", rid)
	}

	// The masked placeholder is present; the token, plaintext, and ciphertext
	// are all absent.
	if !strings.Contains(indexed, indexTestPlaceholder) {
		t.Errorf("indexed content %q missing masked placeholder", indexed)
	}
	if strings.Contains(indexed, plaintext) {
		t.Errorf("indexed content %q leaked plaintext", indexed)
	}
	if strings.Contains(indexed, ciphertext) {
		t.Errorf("indexed content %q leaked ciphertext", indexed)
	}
	if strings.Contains(indexed, "|encrypt|tok-123|") {
		t.Errorf("indexed content %q leaked the raw token", indexed)
	}

	// The non-sensitive surrounding words remain searchable...
	if got := ftsMatch(t, f, "bravo"); len(got) != 1 || got[0] != rid {
		t.Errorf("match bravo = %v, want [%s]", got, rid)
	}
	// ...while the plaintext is not indexed and cannot match.
	if got := ftsMatch(t, f, plaintext); len(got) != 0 {
		t.Errorf("match plaintext = %v, want none", got)
	}
}

// TestIndexPolygonKeepsIndexInSyncOnEdit confirms re-indexing after an edit
// replaces the previous content: the old text no longer matches and the new
// text does, with exactly one row per Polygon.
func TestIndexPolygonKeepsIndexInSyncOnEdit(t *testing.T) {
	f := newIndexFixture(t)
	ctx := context.Background()

	rid := recordid.New().Canonical()

	if err := f.index.IndexPolygon(ctx, f.rcA, rid, "original delta content"); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	if got := ftsMatch(t, f, "delta"); len(got) != 1 || got[0] != rid {
		t.Fatalf("pre-edit match delta = %v, want [%s]", got, rid)
	}

	// Edit: content changes entirely.
	if err := f.index.IndexPolygon(ctx, f.rcA, rid, "revised omega content"); err != nil {
		t.Fatalf("re-index: %v", err)
	}

	// The old term no longer matches; the new one does.
	if got := ftsMatch(t, f, "delta"); len(got) != 0 {
		t.Errorf("post-edit match delta = %v, want none", got)
	}
	if got := ftsMatch(t, f, "omega"); len(got) != 1 || got[0] != rid {
		t.Errorf("post-edit match omega = %v, want [%s]", got, rid)
	}

	// Exactly one index row for the Polygon (upsert, not append).
	var count int
	if err := f.dbA.QueryRow(
		`SELECT COUNT(*) FROM polygon_fts WHERE record_id = ?`, rid,
	).Scan(&count); err != nil {
		t.Fatalf("count index rows: %v", err)
	}
	if count != 1 {
		t.Errorf("index rows for %q = %d, want 1", rid, count)
	}
}

// TestRemoveFromIndexOnDelete confirms deleting a Polygon's index row removes it
// from the index so it no longer matches (keeping the index in sync on delete).
func TestRemoveFromIndexOnDelete(t *testing.T) {
	f := newIndexFixture(t)
	ctx := context.Background()

	rid := recordid.New().Canonical()
	if err := f.index.IndexPolygon(ctx, f.rcA, rid, "gamma epsilon content"); err != nil {
		t.Fatalf("index: %v", err)
	}
	if got := ftsMatch(t, f, "epsilon"); len(got) != 1 {
		t.Fatalf("pre-delete match epsilon = %v, want one", got)
	}

	if err := f.index.RemoveFromIndex(ctx, f.rcA, rid); err != nil {
		t.Fatalf("remove from index: %v", err)
	}

	if _, ok := ftsContent(t, f, rid); ok {
		t.Errorf("index row for %q still present after delete", rid)
	}
	if got := ftsMatch(t, f, "epsilon"); len(got) != 0 {
		t.Errorf("post-delete match epsilon = %v, want none", got)
	}
}

// TestRemoveFromIndexUnindexedIsNoOp confirms removing a Polygon that was never
// indexed is a harmless no-op, so delete handlers can call it unconditionally.
func TestRemoveFromIndexUnindexedIsNoOp(t *testing.T) {
	f := newIndexFixture(t)
	ctx := context.Background()

	if err := f.index.RemoveFromIndex(ctx, f.rcA, recordid.New().Canonical()); err != nil {
		t.Fatalf("remove unindexed polygon: %v", err)
	}
}

// TestIndexPolygonNilMaskRefuses confirms an index configured without a masker
// refuses to write rather than risk indexing raw content (defense in depth for
// the non-leakage guarantee).
func TestIndexPolygonNilMaskRefuses(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()
	idx := &PolygonIndex{Base: f.base, mask: nil}

	if err := idx.IndexPolygon(ctx, f.rcA, recordid.New().Canonical(), "content"); err == nil {
		t.Fatal("expected error indexing with nil mask, got nil")
	}
}
