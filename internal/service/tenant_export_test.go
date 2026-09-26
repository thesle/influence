package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/influence/influence/internal/data"
)

// TestExportTenantProducesSelfSufficientCopy asserts the happy path (Req 2.2,
// 16.9): exporting a created tenant yields a valid, openable SQLite file that
// is a migrated tenant DB, still carries its TENANT_SECRET, and whose DEK
// unwraps with the same server master secret — proving the export is
// self-sufficient and decryptable on a successor host.
func TestExportTenantProducesSelfSufficientCopy(t *testing.T) {
	ctx := context.Background()
	svc, _, dir := newTenantServiceFixture(t)

	id, err := svc.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	dest := filepath.Join(dir, "export.db")
	if err := svc.ExportTenant(ctx, id, dest); err != nil {
		t.Fatalf("ExportTenant: %v", err)
	}

	// The export file exists on disk.
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("export file not created: %v", err)
	}

	// It opens as a valid SQLite database at the current tenant schema version.
	exp, err := sql.Open(data.DriverName, dest)
	if err != nil {
		t.Fatalf("open export: %v", err)
	}
	defer func() { _ = exp.Close() }()

	var check string
	if err := exp.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		t.Fatalf("integrity check export: %v", err)
	}
	if check != "ok" {
		t.Fatalf("export failed integrity check: %s", check)
	}

	var version int
	if err := exp.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read export schema version: %v", err)
	}
	if version != data.TenantSchemaVersion {
		t.Errorf("export schema version = %d, want %d", version, data.TenantSchemaVersion)
	}

	// It still contains the TENANT_SECRET row (Tenant_Salt + wrapped key).
	var secrets int
	if err := exp.QueryRowContext(ctx, "SELECT COUNT(*) FROM tenant_secret").Scan(&secrets); err != nil {
		t.Fatalf("count tenant_secret in export: %v", err)
	}
	if secrets != 1 {
		t.Fatalf("export tenant_secret rows = %d, want 1", secrets)
	}

	// The DEK unwraps with the same master secret used at creation — the export
	// is decryptable on a successor host that holds the master secret (Req
	// 2.2, 16.9).
	dek, err := UnwrapDEK(ctx, exp, "server-master-secret")
	if err != nil {
		t.Fatalf("UnwrapDEK on export: %v", err)
	}
	if len(dek) == 0 {
		t.Fatal("expected a non-empty DEK from the export")
	}
}

// TestExportTenantPreservesLiveKeyMaterial asserts the exported DEK is
// identical to the live tenant's DEK, so ciphertext encrypted before export
// remains decryptable after import (Req 16.9).
func TestExportTenantPreservesLiveKeyMaterial(t *testing.T) {
	ctx := context.Background()
	svc, central, dir := newTenantServiceFixture(t)

	id, err := svc.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	// Read the live tenant's DEK directly from its source file.
	var srcPath string
	if err := central.QueryRowContext(ctx,
		`SELECT db_path FROM "TENANT" WHERE id = ?`, int64(id),
	).Scan(&srcPath); err != nil {
		t.Fatalf("resolve source path: %v", err)
	}
	srcDB, err := sql.Open(data.DriverName, srcPath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer func() { _ = srcDB.Close() }()
	liveDEK, err := UnwrapDEK(ctx, srcDB, "server-master-secret")
	if err != nil {
		t.Fatalf("UnwrapDEK on source: %v", err)
	}

	dest := filepath.Join(dir, "export.db")
	if err := svc.ExportTenant(ctx, id, dest); err != nil {
		t.Fatalf("ExportTenant: %v", err)
	}
	exp, err := sql.Open(data.DriverName, dest)
	if err != nil {
		t.Fatalf("open export: %v", err)
	}
	defer func() { _ = exp.Close() }()
	expDEK, err := UnwrapDEK(ctx, exp, "server-master-secret")
	if err != nil {
		t.Fatalf("UnwrapDEK on export: %v", err)
	}

	if string(liveDEK) != string(expDEK) {
		t.Error("exported DEK differs from live DEK; ciphertext would not decrypt after import")
	}
}

// TestExportTenantUnknownTenantAborts asserts export of a missing/unknown
// tenant aborts with an error and writes no file (Req 2.4).
func TestExportTenantUnknownTenantAborts(t *testing.T) {
	ctx := context.Background()
	svc, _, dir := newTenantServiceFixture(t)

	dest := filepath.Join(dir, "export.db")
	err := svc.ExportTenant(ctx, data.TenantID(9999), dest)
	if err == nil {
		t.Fatal("expected ExportTenant to fail for an unknown tenant")
	}
	if !errors.Is(err, data.ErrTenantNotFound) {
		t.Errorf("error = %v, want ErrTenantNotFound", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("expected no export file for a failed export, stat err = %v", statErr)
	}
}

// TestExportTenantMissingSourceAborts asserts that a registry row whose backing
// file was removed aborts the export with an error and produces no file (Req
// 2.4).
func TestExportTenantMissingSourceAborts(t *testing.T) {
	ctx := context.Background()
	svc, central, dir := newTenantServiceFixture(t)

	id, err := svc.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	// Delete the source file out from under the registry row.
	var srcPath string
	if err := central.QueryRowContext(ctx,
		`SELECT db_path FROM "TENANT" WHERE id = ?`, int64(id),
	).Scan(&srcPath); err != nil {
		t.Fatalf("resolve source path: %v", err)
	}
	if err := os.Remove(srcPath); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	dest := filepath.Join(dir, "export.db")
	if err := svc.ExportTenant(ctx, id, dest); err == nil {
		t.Fatal("expected ExportTenant to fail when the source file is missing")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("expected no export file for a failed export, stat err = %v", statErr)
	}
}

// TestExportTenantCorruptSourceAborts asserts that a corrupt source database
// aborts the export with an error and produces no file (Req 2.4).
func TestExportTenantCorruptSourceAborts(t *testing.T) {
	ctx := context.Background()
	svc, central, dir := newTenantServiceFixture(t)

	id, err := svc.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	// Overwrite the source file with non-SQLite bytes to simulate corruption.
	var srcPath string
	if err := central.QueryRowContext(ctx,
		`SELECT db_path FROM "TENANT" WHERE id = ?`, int64(id),
	).Scan(&srcPath); err != nil {
		t.Fatalf("resolve source path: %v", err)
	}
	if err := os.WriteFile(srcPath, []byte("not a sqlite database at all"), 0o600); err != nil {
		t.Fatalf("corrupt source: %v", err)
	}

	dest := filepath.Join(dir, "export.db")
	if err := svc.ExportTenant(ctx, id, dest); err == nil {
		t.Fatal("expected ExportTenant to fail when the source is corrupt")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("expected no export file for a failed export, stat err = %v", statErr)
	}
}

// TestExportTenantEmptyDestinationRejected asserts an empty destination is
// rejected up front.
func TestExportTenantEmptyDestinationRejected(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTenantServiceFixture(t)

	id, err := svc.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := svc.ExportTenant(ctx, id, ""); !errors.Is(err, ErrExportDestinationRequired) {
		t.Fatalf("error = %v, want ErrExportDestinationRequired", err)
	}
}

// TestExportTenantExistingDestinationRejected asserts export refuses to
// overwrite an existing destination file, so a stale file is never mistaken
// for a fresh export.
func TestExportTenantExistingDestinationRejected(t *testing.T) {
	ctx := context.Background()
	svc, _, dir := newTenantServiceFixture(t)

	id, err := svc.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	dest := filepath.Join(dir, "export.db")
	if err := os.WriteFile(dest, []byte("preexisting"), 0o600); err != nil {
		t.Fatalf("write preexisting dest: %v", err)
	}
	if err := svc.ExportTenant(ctx, id, dest); err == nil {
		t.Fatal("expected ExportTenant to fail when the destination already exists")
	}
}
