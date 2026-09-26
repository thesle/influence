package repo

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"database/sql"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// twoTenantFixture stands up two real, file-backed tenants behind a real
// ConnManager driven by a real Central_Directory registry. This exercises the
// exact production tenant-routing path (NewConnManager -> centralTenantRegistry
// -> per-tenant SQLite file) rather than a fake, so tenant isolation is proven
// end to end.
type twoTenantFixture struct {
	conns  data.ConnManager
	base   Base
	rcA    data.RequestContext
	rcB    data.RequestContext
	dbA    *sql.DB
	dbB    *sql.DB
}

func newTwoTenantFixture(t *testing.T) twoTenantFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	// Central_Directory with two active tenants pointing at their own files.
	central, err := sql.Open(data.DriverName, filepath.Join(dir, "central.db"))
	if err != nil {
		t.Fatalf("open central: %v", err)
	}
	if err := data.MigrateCentral(ctx, central); err != nil {
		t.Fatalf("migrate central: %v", err)
	}

	pathA := filepath.Join(dir, "tenantA.db")
	pathB := filepath.Join(dir, "tenantB.db")
	insertTenant(t, central, 1, "tenant-a-uuid", "A", pathA)
	insertTenant(t, central, 2, "tenant-b-uuid", "B", pathB)

	dbA := migrateTenantFile(t, pathA)
	dbB := migrateTenantFile(t, pathB)

	conns := data.NewConnManager(central)
	t.Cleanup(func() { _ = conns.Close() })

	return twoTenantFixture{
		conns: conns,
		base:  NewBase(conns),
		rcA:   data.NewRequestContext(10, 1, nil, nil),
		rcB:   data.NewRequestContext(20, 2, nil, nil),
		dbA:   dbA,
		dbB:   dbB,
	}
}

func insertTenant(t *testing.T, central *sql.DB, id int64, uuid, name, path string) {
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

// migrateTenantFile creates and migrates a tenant SQLite file, returning an
// open handle the test uses to seed rows directly. The ConnManager opens its
// own pool against the same file.
func migrateTenantFile(t *testing.T, path string) *sql.DB {
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

// seedSphere inserts a Sphere row with the given Record_ID directly into a
// tenant DB and returns the assigned surrogate id.
func seedSphere(t *testing.T, db *sql.DB, id recordid.ID, name string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO sphere (record_id, name, created_at) VALUES (?, ?, '2024-01-01T00:00:00Z')`,
		id.Canonical(), name,
	)
	if err != nil {
		t.Fatalf("seed sphere: %v", err)
	}
	surrogate, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return surrogate
}

// TestResolveRecordFindsRecordInOwnTenant confirms a Record_ID present in the
// caller's tenant resolves to its surrogate id.
func TestResolveRecordFindsRecordInOwnTenant(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	rid := recordid.New()
	want := seedSphere(t, f.dbA, rid, "Sphere One")

	got, err := f.base.resolveRecord(ctx, f.rcA, RecordSphere, rid)
	if err != nil {
		t.Fatalf("resolveRecord in own tenant: %v", err)
	}
	if got != want {
		t.Errorf("surrogate id = %d, want %d", got, want)
	}
}

// TestResolveRecordCrossTenantIsNotAccessible is the core isolation guarantee: a
// Record_ID that exists ONLY in tenant B is not accessible from tenant A's
// context, and vice versa. There is no cross-tenant query path (Req 1.5, 1.6).
func TestResolveRecordCrossTenantIsNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	// The record lives in tenant B only.
	rid := recordid.New()
	seedSphere(t, f.dbB, rid, "Only In B")

	// Tenant A cannot see it — indistinguishable from "does not exist".
	_, err := f.base.resolveRecord(ctx, f.rcA, RecordSphere, rid)
	if !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant resolve: got %v, want ErrNotAccessible", err)
	}

	// Tenant B, which owns it, resolves it fine.
	if _, err := f.base.resolveRecord(ctx, f.rcB, RecordSphere, rid); err != nil {
		t.Fatalf("owning tenant resolve: %v", err)
	}
}

// TestResolveRecordMissingIsNotAccessible confirms a Record_ID absent from every
// tenant returns ErrNotAccessible rather than a raw not-found error.
func TestResolveRecordMissingIsNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	_, err := f.base.resolveRecord(ctx, f.rcA, RecordSphere, recordid.New())
	if !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("missing record: got %v, want ErrNotAccessible", err)
	}
}

// TestResolveRecordDoesNotMutate confirms a not-accessible lookup changes no
// data in either tenant (Req 1.6 — "no data change").
func TestResolveRecordDoesNotMutate(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	rid := recordid.New()
	seedSphere(t, f.dbB, rid, "Only In B")

	before := countSpheres(t, f.dbA) + countSpheres(t, f.dbB)

	// A cross-tenant attempt from A must not add, remove, or alter any row.
	_, _ = f.base.resolveRecord(ctx, f.rcA, RecordSphere, rid)

	after := countSpheres(t, f.dbA) + countSpheres(t, f.dbB)
	if before != after {
		t.Errorf("sphere count changed by a not-accessible lookup: before=%d after=%d", before, after)
	}
}

func countSpheres(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sphere`).Scan(&n); err != nil {
		t.Fatalf("count spheres: %v", err)
	}
	return n
}

// TestResolveRecordUnknownTenantIsNotAccessible confirms a context naming a
// tenant with no active registry entry surfaces as ErrNotAccessible, so a
// missing tenant is indistinguishable from a missing record.
func TestResolveRecordUnknownTenantIsNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	rcUnknown := data.NewRequestContext(99, 404, nil, nil)
	_, err := f.base.resolveRecord(ctx, rcUnknown, RecordSphere, recordid.New())
	if !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("unknown tenant: got %v, want ErrNotAccessible", err)
	}
}

// TestResolveRecordUnknownTypeIsNotAccessible confirms an out-of-allow-list
// record type never runs a query and returns ErrNotAccessible.
func TestResolveRecordUnknownTypeIsNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	_, err := f.base.resolveRecordCanonical(ctx, f.rcA, RecordType("secrets"), recordid.New().Canonical())
	if !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("unknown type: got %v, want ErrNotAccessible", err)
	}
}
