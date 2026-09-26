package data

import (
	"context"
	"database/sql"
	"sort"
	"testing"

	"pgregory.net/rapid"
)

// openTenantDB opens a fresh in-memory Tenant_Database. Each call gets its own
// isolated database because the DSN uses a shared-nothing in-memory file.
func openTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(DriverName, ":memory:")
	if err != nil {
		t.Fatalf("open tenant db: %v", err)
	}
	// A single connection keeps the in-memory database alive for the test.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// tenantTables is every table/virtual table the migration is required to
// create (task 3.2). FTS5 also creates shadow tables (polygon_fts_*), which we
// ignore — we assert the required set is a subset of what exists.
var tenantTables = []string{
	"sphere", "facet", "polygon", "ciphertext", "image_blob",
	"sphere_grant", "tenant_secret", "edit_lock", "user_circle",
	"bookmark", "view_log", "polygon_fts",
}

func objectNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan name: %v", err)
		}
		names[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return names
}

// TestMigrateTenantCreatesAllTables verifies the migration produces every
// required table plus the polygon_fts FTS5 virtual table (Req 23.2 relies on
// FTS5 being present).
func TestMigrateTenantCreatesAllTables(t *testing.T) {
	db := openTenantDB(t)
	if err := MigrateTenant(context.Background(), db); err != nil {
		t.Fatalf("MigrateTenant: %v", err)
	}
	names := objectNames(t, db)
	for _, want := range tenantTables {
		if !names[want] {
			t.Errorf("missing required object %q after migration", want)
		}
	}
}

// TestMigrateTenantSetsVersion asserts the schema version is recorded so a
// second run is a no-op.
func TestMigrateTenantSetsVersion(t *testing.T) {
	db := openTenantDB(t)
	if err := MigrateTenant(context.Background(), db); err != nil {
		t.Fatalf("MigrateTenant: %v", err)
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != TenantSchemaVersion {
		t.Fatalf("user_version = %d, want %d", v, TenantSchemaVersion)
	}
}

// TestMigrateTenantIdempotent asserts re-running the migration succeeds and
// does not error or duplicate objects.
func TestMigrateTenantIdempotent(t *testing.T) {
	db := openTenantDB(t)
	ctx := context.Background()
	if err := MigrateTenant(ctx, db); err != nil {
		t.Fatalf("first MigrateTenant: %v", err)
	}
	before := objectNames(t, db)
	if err := MigrateTenant(ctx, db); err != nil {
		t.Fatalf("second MigrateTenant: %v", err)
	}
	after := objectNames(t, db)
	if len(before) != len(after) {
		t.Fatalf("object count changed on re-migration: %d -> %d", len(before), len(after))
	}
}

// TestRecordIDUniqueConstraint asserts the UNIQUE record_id columns for
// Sphere/Facet/Polygon are enforced by the schema (Req 6.3, 8.3, 10.4, 15.4).
func TestRecordIDUniqueConstraint(t *testing.T) {
	db := openTenantDB(t)
	ctx := context.Background()
	if err := MigrateTenant(ctx, db); err != nil {
		t.Fatalf("MigrateTenant: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO sphere (record_id, name, created_at) VALUES ('rid-1', 'A', '2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("first sphere insert: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sphere (record_id, name, created_at) VALUES ('rid-1', 'B', '2024-01-01T00:00:00Z')`); err == nil {
		t.Fatal("expected UNIQUE violation on duplicate sphere record_id, got nil")
	}
}

// TestSphereCascadeDeletesChildren asserts foreign keys are enabled and the
// ON DELETE CASCADE from Sphere to Facet/Polygon holds (supports Req 6.5 at the
// schema level).
func TestSphereCascadeDeletesChildren(t *testing.T) {
	db := openTenantDB(t)
	ctx := context.Background()
	if err := MigrateTenant(ctx, db); err != nil {
		t.Fatalf("MigrateTenant: %v", err)
	}

	res, err := db.ExecContext(ctx,
		`INSERT INTO sphere (record_id, name, created_at) VALUES ('s1', 'S', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert sphere: %v", err)
	}
	sphereID, _ := res.LastInsertId()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO facet (record_id, sphere_id, name) VALUES ('f1', ?, 'F')`, sphereID); err != nil {
		t.Fatalf("insert facet: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO polygon (record_id, sphere_id, type, author_user_id, created_at, updated_at)
		 VALUES ('p1', ?, 'markdown', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, sphereID); err != nil {
		t.Fatalf("insert polygon: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM sphere WHERE id = ?`, sphereID); err != nil {
		t.Fatalf("delete sphere: %v", err)
	}

	for _, tbl := range []string{"facet", "polygon"} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("expected %s rows cascaded to 0, got %d", tbl, n)
		}
	}
}

// TestPolygonFTSUsable asserts the polygon_fts virtual table indexes and
// matches on masked content (Req 23.2 depends on FTS5 working here).
func TestPolygonFTSUsable(t *testing.T) {
	db := openTenantDB(t)
	ctx := context.Background()
	if err := MigrateTenant(ctx, db); err != nil {
		t.Fatalf("MigrateTenant: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO polygon_fts (record_id, content) VALUES ('p1', 'alpha bravo charlie')`); err != nil {
		t.Fatalf("insert into fts: %v", err)
	}
	var rid string
	if err := db.QueryRowContext(ctx,
		`SELECT record_id FROM polygon_fts WHERE polygon_fts MATCH 'bravo'`).Scan(&rid); err != nil {
		t.Fatalf("fts match query: %v", err)
	}
	if rid != "p1" {
		t.Fatalf("fts match returned %q, want p1", rid)
	}
}

// TestMigrateTenantAllTablesProperty is a property-based check: no matter how
// many times the migration is applied (in any repetition count), the required
// table set is always fully present and stable. This guards the idempotence and
// completeness of the schema across repeated application.
func TestMigrateTenantAllTablesProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		runs := rapid.IntRange(1, 4).Draw(rt, "runs")
		db := openTenantDB(t)
		ctx := context.Background()
		for i := 0; i < runs; i++ {
			if err := MigrateTenant(ctx, db); err != nil {
				rt.Fatalf("MigrateTenant run %d: %v", i, err)
			}
		}
		names := objectNames(t, db)
		got := make([]string, 0, len(tenantTables))
		for _, want := range tenantTables {
			if names[want] {
				got = append(got, want)
			}
		}
		want := append([]string(nil), tenantTables...)
		sort.Strings(want)
		sort.Strings(got)
		if len(got) != len(want) {
			rt.Fatalf("required tables present = %v, want all of %v", got, want)
		}
	})
}
