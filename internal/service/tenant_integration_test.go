package service

// Integration tests for tenant creation rollback (Req 1.8) and export snapshot
// abort (Req 2.4), task 20.4.
//
// These complement the focused unit tests already in tenant_test.go
// (TestCreateTenantRollsBackOn{Build,Migrate}Failure) and tenant_export_test.go
// (unknown/missing/corrupt-source aborts) by exercising the net-new gaps:
//
//   - A LOCKED source file aborts the export. Req 2.4 explicitly names a
//     "locked" file alongside missing and corrupt; the existing export tests
//     cover missing and corrupt but not locked. We hold a real exclusive write
//     lock on the source and assert ExportTenant aborts and writes nothing.
//   - An end-to-end assertion that after a create-rollback the Central_Directory
//     holds NO registry row of ANY status and the Data_Directory holds NO tenant
//     file, and that the system remains usable afterwards (a subsequent create
//     succeeds cleanly). This proves the all-or-nothing guarantee of Req 1.8 at
//     the integration level, not just that a single failed call cleaned up.

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/influence/influence/internal/data"
)

// tenantDBFiles returns the names of tenant *.db files in dir, excluding the
// Central_Directory database. It lets a test assert the Data_Directory holds no
// tenant file after a rollback.
func tenantDBFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".db" && name != "central_directory.db" {
			files = append(files, name)
		}
	}
	return files
}

// TestExportTenantLockedSourceAborts asserts the "locked" branch of Req 2.4:
// when the tenant's source database is locked by an exclusive writer, the
// export cannot read a consistent snapshot, so ExportTenant aborts with an
// error and leaves no destination file behind.
//
// We simulate a locked file the way it actually happens in SQLite: a
// concurrent connection holds an exclusive-lock transaction on the source. The
// export's integrity check (and, failing that, the snapshot) then cannot
// acquire the lock and surfaces SQLITE_BUSY, which ExportTenant reports as an
// export failure.
func TestExportTenantLockedSourceAborts(t *testing.T) {
	ctx := context.Background()
	svc, central, dir := newTenantServiceFixture(t)

	id, err := svc.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	// Resolve the live source file path from the registry.
	var srcPath string
	if err := central.QueryRowContext(ctx,
		`SELECT db_path FROM "TENANT" WHERE id = ?`, int64(id),
	).Scan(&srcPath); err != nil {
		t.Fatalf("resolve source path: %v", err)
	}

	// Hold an exclusive write lock on the source for the duration of the export
	// attempt. A single pinned connection with BEGIN EXCLUSIVE acquires the
	// database-level write lock that VACUUM/quick_check must contend for.
	locker, err := sql.Open(data.DriverName, srcPath)
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	locker.SetMaxOpenConns(1)
	defer func() { _ = locker.Close() }()
	conn, err := locker.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire locker conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("acquire exclusive lock: %v", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()

	// Bound the export so a hypothetical lock-wait can never hang the test.
	exportCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	dest := filepath.Join(dir, "export.db")
	if err := svc.ExportTenant(exportCtx, id, dest); err == nil {
		t.Fatal("expected ExportTenant to fail when the source file is locked")
	}

	// No partial export was left behind (Req 2.4).
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("expected no export file for a locked-source export, stat err = %v", statErr)
	}
}

// TestCreateTenantRollbackLeavesNoTraceThenSucceeds is an end-to-end assertion
// of the all-or-nothing guarantee in Req 1.8: after a DB-file-creation failure
// rolls the saga back, the Central_Directory holds NO registry row of ANY
// status and the Data_Directory holds NO tenant file — and the service remains
// usable, so a subsequent create succeeds and produces exactly one active
// tenant with exactly one tenant file.
func TestCreateTenantRollbackLeavesNoTraceThenSucceeds(t *testing.T) {
	ctx := context.Background()
	svc, central, dir := newTenantServiceFixture(t)

	// Force the DB-file-creation step to fail deterministically via the
	// openTenantFile seam, driving the rollback path.
	orig := openTenantFile
	openTenantFile = func(path string) (*sql.DB, error) {
		return nil, errors.New("forced file-creation failure")
	}

	_, err := svc.CreateTenant(ctx, "DoomedTenant")
	if err == nil {
		// Restore the seam before failing so the deferred restore is not needed.
		openTenantFile = orig
		t.Fatal("expected CreateTenant to fail when the tenant file cannot be created")
	}

	// After rollback: NO registry row of ANY status remains.
	if n := countTenants(t, ctx, central, "pending"); n != 0 {
		t.Errorf("after rollback: pending rows = %d, want 0", n)
	}
	if n := countTenants(t, ctx, central, "active"); n != 0 {
		t.Errorf("after rollback: active rows = %d, want 0", n)
	}
	// Belt-and-suspenders: no row of any status at all.
	var total int
	if err := central.QueryRowContext(ctx, `SELECT COUNT(*) FROM "TENANT"`).Scan(&total); err != nil {
		t.Fatalf("count all tenants: %v", err)
	}
	if total != 0 {
		t.Errorf("after rollback: total registry rows = %d, want 0", total)
	}

	// After rollback: NO tenant file remains in the Data_Directory.
	if files := tenantDBFiles(t, dir); len(files) != 0 {
		t.Errorf("after rollback: stray tenant files %v, want none", files)
	}

	// Restore the seam: the service must remain usable after a rolled-back
	// create.
	openTenantFile = orig

	id, err := svc.CreateTenant(ctx, "Recovered")
	if err != nil {
		t.Fatalf("CreateTenant after rollback: %v", err)
	}

	// Exactly one active tenant now exists, and it is the one we just created.
	if n := countTenants(t, ctx, central, "active"); n != 1 {
		t.Errorf("after recovery: active rows = %d, want 1", n)
	}
	if n := countTenants(t, ctx, central, "pending"); n != 0 {
		t.Errorf("after recovery: pending rows = %d, want 0", n)
	}

	var status, dbPath string
	if err := central.QueryRowContext(ctx,
		`SELECT status, db_path FROM "TENANT" WHERE id = ?`, int64(id),
	).Scan(&status, &dbPath); err != nil {
		t.Fatalf("read recovered registry row: %v", err)
	}
	if status != "active" {
		t.Errorf("recovered tenant status = %q, want active", status)
	}

	// Exactly one tenant file exists, and it backs the recovered tenant.
	files := tenantDBFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("after recovery: tenant files %v, want exactly 1", files)
	}
	if got := filepath.Join(dir, files[0]); got != dbPath {
		t.Errorf("recovered tenant file = %q, want registry db_path %q", got, dbPath)
	}
}
