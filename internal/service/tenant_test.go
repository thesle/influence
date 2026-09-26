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

// newTenantServiceFixture stands up a real, file-backed Central_Directory and a
// real Data_Directory, then returns a TenantService wired to them (via a real
// ConnManager) plus the shared central handle and the data dir so tests can
// inspect the registry and the filesystem after a create/rollback.
func newTenantServiceFixture(t *testing.T) (*TenantService, *sql.DB, string) {
	t.Helper()
	ctx := context.Background()

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

	svc := NewTenantService(conns, dir, "server-master-secret")
	return svc, central, dir
}

// countTenants reports how many registry rows match the given status.
func countTenants(t *testing.T, ctx context.Context, central *sql.DB, status string) int {
	t.Helper()
	var n int
	if err := central.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "TENANT" WHERE status = ?`, status,
	).Scan(&n); err != nil {
		t.Fatalf("count tenants(%s): %v", status, err)
	}
	return n
}

// TestCreateTenantSuccess asserts the happy path (Req 1.7): a successful create
// yields an active registry row, a migrated tenant file on disk, and
// initialized TENANT_SECRET key material.
func TestCreateTenantSuccess(t *testing.T) {
	ctx := context.Background()
	svc, central, dir := newTenantServiceFixture(t)

	id, err := svc.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected a positive tenant id, got %d", id)
	}

	// The registry row exists and is active, with a db_path inside the data dir.
	var status, dbPath, name string
	err = central.QueryRowContext(ctx,
		`SELECT status, db_path, name FROM "TENANT" WHERE id = ?`, int64(id),
	).Scan(&status, &dbPath, &name)
	if err != nil {
		t.Fatalf("read registry row: %v", err)
	}
	if status != "active" {
		t.Errorf("status = %q, want active", status)
	}
	if name != "Acme" {
		t.Errorf("name = %q, want Acme", name)
	}
	if filepath.Dir(dbPath) != dir {
		t.Errorf("db_path %q is not inside the data dir %q", dbPath, dir)
	}

	// No pending rows remain.
	if p := countTenants(t, ctx, central, "pending"); p != 0 {
		t.Errorf("expected 0 pending rows, got %d", p)
	}

	// The tenant file exists and is a migrated tenant DB with initialized key
	// material: opening it and unwrapping the DEK with the same master secret
	// must succeed.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("tenant file not created: %v", err)
	}
	tdb, err := sql.Open(data.DriverName, dbPath)
	if err != nil {
		t.Fatalf("open created tenant file: %v", err)
	}
	defer func() { _ = tdb.Close() }()

	// A migrated file is at the current tenant schema version.
	var version int
	if err := tdb.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read tenant schema version: %v", err)
	}
	if version != data.TenantSchemaVersion {
		t.Errorf("tenant schema version = %d, want %d", version, data.TenantSchemaVersion)
	}

	// TENANT_SECRET was initialized: the DEK unwraps with the server master
	// secret used at creation.
	dek, err := UnwrapDEK(ctx, tdb, "server-master-secret")
	if err != nil {
		t.Fatalf("UnwrapDEK on created tenant: %v", err)
	}
	if len(dek) == 0 {
		t.Fatal("expected a non-empty DEK from the created tenant")
	}
}

// TestCreateTenantActiveTenantIsRoutable asserts a freshly created tenant is
// reachable through the connection manager (its registry row is active), which
// is the observable outcome of Req 1.7.
func TestCreateTenantActiveTenantIsRoutable(t *testing.T) {
	ctx := context.Background()
	svc, central, _ := newTenantServiceFixture(t)

	id, err := svc.CreateTenant(ctx, "Routable")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	// Rebuild a ConnManager over the same central DB and confirm the tenant
	// resolves — proving the registry entry is active and the file is openable.
	// The fixture owns and closes the shared central handle, so this manager's
	// Close would double-close it; we deliberately do not close it here.
	conns := data.NewConnManager(central)
	rc := data.NewRequestContext(1, data.TenantID(id), nil, nil)
	h, err := conns.Tenant(ctx, rc)
	if err != nil {
		t.Fatalf("resolve created tenant: %v", err)
	}
	if h.TenantID() != data.TenantID(id) {
		t.Errorf("resolved tenant = %d, want %d", h.TenantID(), id)
	}
}

// TestCreateTenantRollsBackOnBuildFailure asserts the saga's compensation
// (Req 1.8): when the tenant file cannot be built, the pending registry row is
// removed and no partial file remains — no trace of the tenant survives.
func TestCreateTenantRollsBackOnBuildFailure(t *testing.T) {
	ctx := context.Background()
	svc, central, dir := newTenantServiceFixture(t)

	// Force step 2 to fail deterministically by making file creation error.
	orig := openTenantFile
	openTenantFile = func(path string) (*sql.DB, error) {
		return nil, errors.New("forced tenant file failure")
	}
	defer func() { openTenantFile = orig }()

	_, err := svc.CreateTenant(ctx, "DoomedTenant")
	if err == nil {
		t.Fatal("expected CreateTenant to fail when the tenant file cannot be built")
	}

	// No registry row of any status remains for the tenant.
	if total := countTenants(t, ctx, central, "pending") + countTenants(t, ctx, central, "active"); total != 0 {
		t.Errorf("expected no registry rows after rollback, got %d", total)
	}

	// No stray tenant file was left behind in the data dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".db" && name != "central_directory.db" {
			t.Errorf("stray tenant file left after rollback: %q", name)
		}
	}
}

// TestCreateTenantRollsBackOnMigrateFailure asserts the same all-or-nothing
// guarantee when the file is created but migration fails: the partial file is
// removed and no registry row remains (Req 1.8).
func TestCreateTenantRollsBackOnMigrateFailure(t *testing.T) {
	ctx := context.Background()
	svc, central, dir := newTenantServiceFixture(t)

	// Open a real handle, but to a path that cannot be migrated: point the file
	// at a read-only, non-SQLite path so MigrateTenant fails after the file is
	// "created". We simulate this by pre-creating a bogus file and returning a
	// handle whose migration will fail.
	orig := openTenantFile
	openTenantFile = func(path string) (*sql.DB, error) {
		// Write a corrupt (non-SQLite) file at the path so it exists on disk
		// and a later os.Remove during rollback has something to delete, then
		// hand back a handle whose migration fails.
		if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
			return nil, err
		}
		db, err := sql.Open(data.DriverName, path)
		if err != nil {
			return nil, err
		}
		return db, nil
	}
	defer func() { openTenantFile = orig }()

	_, err := svc.CreateTenant(ctx, "MigrateFail")
	if err == nil {
		t.Fatal("expected CreateTenant to fail when migration fails")
	}

	if total := countTenants(t, ctx, central, "pending") + countTenants(t, ctx, central, "active"); total != 0 {
		t.Errorf("expected no registry rows after rollback, got %d", total)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".db" && name != "central_directory.db" {
			t.Errorf("stray tenant file left after rollback: %q", name)
		}
	}
}

// TestCreateTenantEmptyNameRejected asserts an empty name is rejected up front
// and no registry row or file is created.
func TestCreateTenantEmptyNameRejected(t *testing.T) {
	ctx := context.Background()
	svc, central, dir := newTenantServiceFixture(t)

	_, err := svc.CreateTenant(ctx, "")
	if !errors.Is(err, ErrTenantNameRequired) {
		t.Fatalf("error = %v, want ErrTenantNameRequired", err)
	}
	if total := countTenants(t, ctx, central, "pending") + countTenants(t, ctx, central, "active"); total != 0 {
		t.Errorf("expected no registry rows, got %d", total)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".db" && e.Name() != "central_directory.db" {
			t.Errorf("unexpected tenant file: %q", e.Name())
		}
	}
}
