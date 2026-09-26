package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// encryptFixture stands up a real, file-backed tenant behind a real ConnManager
// driven by a real Central_Directory registry, with TENANT_SECRET provisioned,
// so EncryptSpan runs over the exact production tenant-routing + key-unwrap path
// rather than a fake.
type encryptFixture struct {
	svc     *EncryptionService
	rc      data.RequestContext
	db      *sql.DB // direct handle to the same tenant file, for seeding/asserting
	tenant2 data.RequestContext
	master  string
}

const encTestMaster = "server-master-secret"

func newEncryptFixture(t *testing.T) encryptFixture {
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
	pathB := filepath.Join(dir, "tenantB.db")
	insertEncTenant(t, central, 1, "tenant-a-uuid", "A", pathA)
	insertEncTenant(t, central, 2, "tenant-b-uuid", "B", pathB)

	dbA := migrateEncTenantFile(t, pathA)
	migrateEncTenantFile(t, pathB) // tenant B exists but is unused except for isolation checks

	// Provision key material so UnwrapDEK can recover the DEK.
	if err := InitTenantSecret(ctx, dbA, encTestMaster); err != nil {
		t.Fatalf("init tenant A secret: %v", err)
	}

	conns := data.NewConnManager(central)
	t.Cleanup(func() { _ = conns.Close() })

	return encryptFixture{
		svc:     NewEncryptionService(conns, encTestMaster),
		rc:      data.NewRequestContext(10, 1, nil, nil),
		db:      dbA,
		tenant2: data.NewRequestContext(20, 2, nil, nil),
		master:  encTestMaster,
	}
}

func insertEncTenant(t *testing.T, central *sql.DB, id int64, uuid, name, path string) {
	t.Helper()
	_, err := central.Exec(
		`INSERT INTO "TENANT" (id, tenant_uuid, name, db_path, status, created_at)
		 VALUES (?, ?, ?, ?, 'active', '2024-01-01T00:00:00Z')`,
		id, uuid, name, path,
	)
	if err != nil {
		t.Fatalf("insert tenant %d: %v", id, err)
	}
}

func migrateEncTenantFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open(data.DriverName, path)
	if err != nil {
		t.Fatalf("open tenant file %s: %v", path, err)
	}
	if err := data.MigrateTenant(ctx, db); err != nil {
		t.Fatalf("migrate tenant file %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedPolygon inserts a Sphere and a Polygon with the given content into the
// tenant DB, returning the Polygon's Record_ID.
func seedPolygon(t *testing.T, db *sql.DB, content string) recordid.ID {
	t.Helper()
	sphereRID := recordid.New()
	res, err := db.Exec(
		`INSERT INTO sphere (record_id, name, created_at) VALUES (?, 'S', '2024-01-01T00:00:00Z')`,
		sphereRID.Canonical(),
	)
	if err != nil {
		t.Fatalf("seed sphere: %v", err)
	}
	sphereID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("sphere last id: %v", err)
	}

	polyRID := recordid.New()
	_, err = db.Exec(
		`INSERT INTO polygon (record_id, sphere_id, type, content, author_user_id, created_at, updated_at)
		 VALUES (?, ?, 'markdown', ?, 10, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		polyRID.Canonical(), sphereID, content,
	)
	if err != nil {
		t.Fatalf("seed polygon: %v", err)
	}
	return polyRID
}

func readContent(t *testing.T, db *sql.DB, polyRID recordid.ID) string {
	t.Helper()
	var content string
	if err := db.QueryRow(`SELECT content FROM polygon WHERE record_id = ?`, polyRID.Canonical()).Scan(&content); err != nil {
		t.Fatalf("read content: %v", err)
	}
	return content
}

// TestEncryptSpanReplacesSpanWithToken verifies the core happy path: the span
// is replaced by |encrypt|{id}|, the plaintext no longer appears in stored
// content, and a matching CIPHERTEXT row is written (Req 16.1, 16.2).
func TestEncryptSpanReplacesSpanWithToken(t *testing.T) {
	f := newEncryptFixture(t)
	ctx := context.Background()

	const content = "public SECRET public"
	polyRID := seedPolygon(t, f.db, content)

	start := strings.Index(content, "SECRET")
	end := start + len("SECRET")

	res, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), start, end)
	if err != nil {
		t.Fatalf("EncryptSpan: %v", err)
	}

	wantToken := "|encrypt|" + res.TokenID + "|"
	wantContent := "public " + wantToken + " public"
	if res.NewContent != wantContent {
		t.Errorf("returned content = %q, want %q", res.NewContent, wantContent)
	}

	stored := readContent(t, f.db, polyRID)
	if stored != wantContent {
		t.Errorf("stored content = %q, want %q", stored, wantContent)
	}
	if strings.Contains(stored, "SECRET") {
		t.Errorf("stored content still contains plaintext: %q", stored)
	}

	// A CIPHERTEXT row keyed by the token id must exist with a 96-bit nonce and
	// non-empty ciphertext, and it must not contain the plaintext bytes.
	var nonce, ct []byte
	if err := f.db.QueryRow(`SELECT nonce, ciphertext FROM ciphertext WHERE token_id = ?`, res.TokenID).Scan(&nonce, &ct); err != nil {
		t.Fatalf("read ciphertext row: %v", err)
	}
	if len(nonce) != gcmNonceLen {
		t.Errorf("nonce length = %d, want %d", len(nonce), gcmNonceLen)
	}
	if len(ct) == 0 {
		t.Error("ciphertext is empty")
	}
	if strings.Contains(string(ct), "SECRET") {
		t.Error("ciphertext row contains plaintext")
	}
}

// TestEncryptSpanRoundTripsUnderDEK verifies the stored ciphertext actually
// decrypts back to the original span under the tenant DEK, proving AES-256-GCM
// was used correctly (Req 16.1).
func TestEncryptSpanRoundTripsUnderDEK(t *testing.T) {
	f := newEncryptFixture(t)
	ctx := context.Background()

	const content = "alpha hidden-data omega"
	polyRID := seedPolygon(t, f.db, content)
	start := strings.Index(content, "hidden-data")
	end := start + len("hidden-data")

	res, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), start, end)
	if err != nil {
		t.Fatalf("EncryptSpan: %v", err)
	}

	var nonce, ct []byte
	if err := f.db.QueryRow(`SELECT nonce, ciphertext FROM ciphertext WHERE token_id = ?`, res.TokenID).Scan(&nonce, &ct); err != nil {
		t.Fatalf("read ciphertext row: %v", err)
	}

	dek, err := UnwrapDEK(ctx, f.db, f.master)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	plain := decryptForTest(t, dek, nonce, ct)
	if string(plain) != "hidden-data" {
		t.Errorf("decrypted = %q, want %q", plain, "hidden-data")
	}
}

// TestEncryptSpanUniqueTokenPerCall verifies two encryptions of the same text
// in the same Polygon get distinct token ids and distinct rows (Req 16.1 unique
// id).
func TestEncryptSpanUniqueTokenPerCall(t *testing.T) {
	f := newEncryptFixture(t)
	ctx := context.Background()

	content := "one two"
	polyRID := seedPolygon(t, f.db, content)

	// Encrypt "one" first.
	r1, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), 0, 3)
	if err != nil {
		t.Fatalf("first EncryptSpan: %v", err)
	}
	// Re-read and encrypt "two" from the updated content.
	updated := readContent(t, f.db, polyRID)
	start := strings.Index(updated, "two")
	r2, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), start, start+3)
	if err != nil {
		t.Fatalf("second EncryptSpan: %v", err)
	}
	if r1.TokenID == r2.TokenID {
		t.Error("expected distinct token ids for two encryptions")
	}

	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM ciphertext`).Scan(&count); err != nil {
		t.Fatalf("count ciphertext: %v", err)
	}
	if count != 2 {
		t.Errorf("ciphertext row count = %d, want 2", count)
	}
}

// TestEncryptSpanInvalidRangeLeavesContentUnchanged verifies out-of-bounds or
// empty spans are rejected with ErrInvalidSpan and write nothing.
func TestEncryptSpanInvalidRangeLeavesContentUnchanged(t *testing.T) {
	f := newEncryptFixture(t)
	ctx := context.Background()

	const content = "unchanged"
	polyRID := seedPolygon(t, f.db, content)

	cases := []struct {
		name       string
		start, end int
	}{
		{"empty range", 2, 2},
		{"reversed", 5, 2},
		{"negative start", -1, 3},
		{"end past content", 0, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), tc.start, tc.end)
			if err != ErrInvalidSpan {
				t.Fatalf("error = %v, want ErrInvalidSpan", err)
			}
			if got := readContent(t, f.db, polyRID); got != content {
				t.Errorf("content changed to %q, want %q", got, content)
			}
		})
	}

	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM ciphertext`).Scan(&count); err != nil {
		t.Fatalf("count ciphertext: %v", err)
	}
	if count != 0 {
		t.Errorf("ciphertext rows written = %d, want 0", count)
	}
}

// TestEncryptSpanUnknownPolygonNotAccessible verifies an unknown/absent Polygon
// Record_ID yields ErrPolygonNotAccessible with no data change (Req 1.6).
func TestEncryptSpanUnknownPolygonNotAccessible(t *testing.T) {
	f := newEncryptFixture(t)
	ctx := context.Background()

	_, err := f.svc.EncryptSpan(ctx, f.rc, recordid.New().Canonical(), 0, 1)
	if err != ErrPolygonNotAccessible {
		t.Fatalf("error = %v, want ErrPolygonNotAccessible", err)
	}
}

// TestEncryptSpanCrossTenantNotAccessible verifies a Polygon that exists only
// in tenant A cannot be encrypted from tenant B's context (Req 1.5, 1.6).
func TestEncryptSpanCrossTenantNotAccessible(t *testing.T) {
	f := newEncryptFixture(t)
	ctx := context.Background()

	polyRID := seedPolygon(t, f.db, "secret text")

	// tenant2 has no such Polygon in its own DB, so it is not accessible.
	_, err := f.svc.EncryptSpan(ctx, f.tenant2, polyRID.Canonical(), 0, 6)
	if err != ErrPolygonNotAccessible {
		t.Fatalf("cross-tenant error = %v, want ErrPolygonNotAccessible", err)
	}

	// The plaintext in tenant A is untouched.
	if got := readContent(t, f.db, polyRID); got != "secret text" {
		t.Errorf("tenant A content changed to %q", got)
	}
}

// TestEncryptSpanMissingSecretPropagates verifies that a tenant without
// provisioned key material surfaces ErrTenantSecretMissing and writes nothing.
func TestEncryptSpanMissingSecretPropagates(t *testing.T) {
	// Build a fixture, then wipe tenant A's TENANT_SECRET so unwrap fails.
	f := newEncryptFixture(t)
	ctx := context.Background()
	if _, err := f.db.Exec(`DELETE FROM tenant_secret`); err != nil {
		t.Fatalf("clear tenant secret: %v", err)
	}

	polyRID := seedPolygon(t, f.db, "some secret")
	_, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), 5, 11)
	if err != ErrTenantSecretMissing {
		t.Fatalf("error = %v, want ErrTenantSecretMissing", err)
	}

	if got := readContent(t, f.db, polyRID); got != "some secret" {
		t.Errorf("content changed to %q on failed unwrap", got)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM ciphertext`).Scan(&count); err != nil {
		t.Fatalf("count ciphertext: %v", err)
	}
	if count != 0 {
		t.Errorf("ciphertext rows = %d, want 0", count)
	}
}

// decryptForTest reverses encryptSpan for verification only.
func decryptForTest(t *testing.T, dek, nonce, ct []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(dek)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("gcm open: %v", err)
	}
	return plain
}
