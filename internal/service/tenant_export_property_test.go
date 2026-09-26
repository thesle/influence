package service

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/influence/influence/internal/data"
	"pgregory.net/rapid"
)

// exportPropertyFixture stands up a real, file-backed tenant provisioned through
// the production TenantService.CreateTenant saga (which mints the Tenant_Salt +
// wrapped DEK inside the tenant file, Req 2.2/16.9), plus an EncryptionService
// wired to the SAME ConnManager and server master secret so encryption routes
// through the exact same live tenant file that ExportTenant later snapshots.
//
// The tenant is created ONCE per fixture: CreateTenant runs an Argon2id KDF to
// derive the tenant KEK, which is deliberately slow, so the property below
// reuses this single tenant across all iterations and only exercises the fast
// encrypt → export → open-exported → unwrap → decrypt path per iteration.
type exportPropertyFixture struct {
	tenantSvc *TenantService
	encSvc    *EncryptionService
	tenantID  data.TenantID
	rc        data.RequestContext
	liveDB    *sql.DB // direct handle to the live tenant file, for seeding polygons
	dir       string  // data dir, used as the parent for per-iteration export paths
	master    string
}

func newExportPropertyFixture(t *testing.T) exportPropertyFixture {
	t.Helper()
	ctx := context.Background()
	const master = "server-master-secret"

	dir := t.TempDir()
	central, err := sql.Open(data.DriverName, filepath.Join(dir, "central_directory.db"))
	if err != nil {
		t.Fatalf("open central: %v", err)
	}
	central.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = central.Close() })
	if err := data.MigrateCentral(ctx, central); err != nil {
		t.Fatalf("migrate central: %v", err)
	}

	conns := data.NewConnManager(central)
	t.Cleanup(func() { _ = conns.Close() })

	tenantSvc := NewTenantService(conns, dir, master)
	encSvc := NewEncryptionService(conns, master)

	// Create the single tenant through the real saga (the slow, Argon2id step).
	id, err := tenantSvc.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	// Resolve the live tenant file path from the registry and open a direct
	// handle for seeding polygons the EncryptionService will encrypt.
	var srcPath string
	if err := central.QueryRowContext(ctx,
		`SELECT db_path FROM "TENANT" WHERE id = ?`, int64(id),
	).Scan(&srcPath); err != nil {
		t.Fatalf("resolve source path: %v", err)
	}
	liveDB, err := sql.Open(data.DriverName, srcPath)
	if err != nil {
		t.Fatalf("open live tenant db: %v", err)
	}
	t.Cleanup(func() { _ = liveDB.Close() })

	return exportPropertyFixture{
		tenantSvc: tenantSvc,
		encSvc:    encSvc,
		tenantID:  id,
		rc:        data.NewRequestContext(10, id, nil, nil),
		liveDB:    liveDB,
		dir:       dir,
		master:    master,
	}
}

// TestEncryptedContentSurvivesExportImportProperty is the property test for the
// export/import durability guarantee (Requirements 2.2, 16.9).
//
// Feature: influence, Property 21: Encrypted content survives tenant export/import
//
// Property 21 (from design.md): For all encrypted spans, exporting a
// Tenant_Database and re-importing it on a fresh host preserves the ability of a
// user with Reveal_Permission to reveal each token and obtain the original
// plaintext.
//
// The test creates a single tenant through the production saga (which embeds the
// Tenant_Salt + wrapped DEK in the tenant file) and, for each arbitrary span:
//
//  1. seeds a Polygon and encrypts the span via the real EncryptionService,
//     producing an Obfuscation_Token backed by a CIPHERTEXT row in the LIVE
//     tenant file;
//  2. exports the tenant via TenantService.ExportTenant, which VACUUM INTOs a
//     self-contained snapshot of that file to a fresh path — simulating hand-off
//     to a successor host;
//  3. opens the EXPORTED file as if it were a freshly imported tenant DB on that
//     successor host, unwraps the DEK straight out of the export's TENANT_SECRET
//     using the SAME server master secret (proving the export carries its own
//     key material — Req 2.2), and decrypts the exported CIPHERTEXT row for the
//     token under that DEK.
//
// It then asserts the decrypted plaintext equals the original span byte-for-byte.
// Because the DEK is recovered from the export's own Tenant_Salt + wrapped key
// (never from the origin host), a successful decrypt proves the encrypted
// content survived the export/import round-trip on a fresh host with nothing but
// the shared master secret (Req 2.2, 16.9). Unwrapping and decrypting the
// exported ciphertext directly is the faithful mechanical equivalent of a
// permitted RevealToken against the re-imported tenant: RevealToken's own
// decryption path is UnwrapDEK + AES-256-GCM open of exactly this nonce+
// ciphertext, and it is separately covered end-to-end by Property 17.
//
// The span is drawn non-empty (EncryptSpan rejects empty ranges) over a broad
// alphabet, and its offsets are computed from the generated prefix/span lengths
// so the encrypted range is always exactly the drawn span.
//
// Validates: Requirements 2.2, 16.9
func TestEncryptedContentSurvivesExportImportProperty(t *testing.T) {
	f := newExportPropertyFixture(t)
	ctx := context.Background()

	// A broad alphabet including letters, digits, spaces, and punctuation so the
	// round-trip is exercised over arbitrary sensitive spans.
	const alphabet = "abcABC 012 .,-_|"

	iter := 0
	rapid.Check(t, func(rt *rapid.T) {
		prefix := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 0, 16, -1).Draw(rt, "prefix")
		span := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 1, 24, -1).Draw(rt, "span")
		suffix := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 0, 16, -1).Draw(rt, "suffix")

		content := prefix + span + suffix
		polyRID := seedPolygon(t, f.liveDB, content)

		start := len(prefix)
		end := start + len(span)

		// Encrypt the span in the live tenant, producing a token + CIPHERTEXT row.
		res, err := f.encSvc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), start, end)
		if err != nil {
			rt.Fatalf("EncryptSpan(content=%q, [%d,%d)) error = %v", content, start, end, err)
		}

		// Export the tenant to a fresh, per-iteration destination. VACUUM INTO
		// refuses to overwrite, so each iteration uses a unique path; a unique
		// name per iteration also avoids any cross-iteration interference.
		iter++
		dest := filepath.Join(f.dir, fmt.Sprintf("export_%d.db", iter))
		if err := f.tenantSvc.ExportTenant(ctx, f.tenantID, dest); err != nil {
			rt.Fatalf("ExportTenant: %v", err)
		}

		// Open the exported file as a freshly imported tenant DB on a successor
		// host and recover the DEK from ITS OWN embedded key material using only
		// the shared master secret — no key delivery from the origin host.
		exp, err := sql.Open(data.DriverName, dest)
		if err != nil {
			rt.Fatalf("open export: %v", err)
		}
		expDEK, err := UnwrapDEK(ctx, exp, f.master)
		if err != nil {
			_ = exp.Close()
			rt.Fatalf("UnwrapDEK on export: %v", err)
		}

		// Read the token's ciphertext straight from the exported file and decrypt
		// it under the export-recovered DEK. This is the mechanical core of a
		// permitted reveal against the re-imported tenant.
		var nonce, ciphertext []byte
		err = exp.QueryRowContext(ctx,
			`SELECT nonce, ciphertext FROM ciphertext WHERE token_id = ?`, res.TokenID,
		).Scan(&nonce, &ciphertext)
		if err != nil {
			_ = exp.Close()
			rt.Fatalf("read exported ciphertext for token %q: %v", res.TokenID, err)
		}

		plainBytes, err := decryptSpan(expDEK, nonce, ciphertext)
		_ = exp.Close()
		if err != nil {
			rt.Fatalf("decrypt exported ciphertext for token %q: %v", res.TokenID, err)
		}

		// The plaintext recovered from the EXPORTED tenant must equal the original
		// span byte-for-byte: encrypted content survived export/import (Req 2.2,
		// 16.9).
		if string(plainBytes) != span {
			rt.Fatalf("recovered plaintext from export = %q, want original span %q (content=%q)",
				string(plainBytes), span, content)
		}
	})
}
