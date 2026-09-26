package api

// Seeding helpers shared by the handler tests. They insert the minimal
// Central_Directory identity rows and tenant content rows the wired handlers
// need, using the same table shapes the production schema/migrations define.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// sqlDB aliases *sql.DB so the test env struct can name it without importing
// database/sql into every signature.
type sqlDB = sql.DB

// openSQLite opens a SQLite handle with foreign keys enabled (so cascade rules
// hold) and registers cleanup.
func openSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(data.DriverName, path)
	if err != nil {
		t.Fatalf("open sqlite %s: %v", filepath.Base(path), err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func insertTenant(t *testing.T, central *sql.DB, id int64, uuid, name, path string) {
	t.Helper()
	if _, err := central.Exec(
		`INSERT INTO "TENANT" (id, tenant_uuid, name, db_path, status, created_at)
		 VALUES (?, ?, ?, ?, 'active', '2024-01-01T00:00:00Z')`,
		id, uuid, name, path,
	); err != nil {
		t.Fatalf("insert tenant %d: %v", id, err)
	}
}

func insertUser(t *testing.T, central *sql.DB, tenantID int64, username, display string) data.UserID {
	t.Helper()
	res, err := central.Exec(
		`INSERT INTO "USER" (tenant_id, username, display_name, password_hash, created_at)
		 VALUES (?, ?, ?, 'x', '2024-01-01T00:00:00Z')`,
		tenantID, username, display,
	)
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user last id: %v", err)
	}
	return data.UserID(id)
}

func insertGroup(t *testing.T, central *sql.DB, tenantID int64, name string, admin bool) data.GroupID {
	t.Helper()
	isAdmin := 0
	if admin {
		isAdmin = 1
	}
	res, err := central.Exec(
		`INSERT INTO "GROUP" (tenant_id, name, is_admin) VALUES (?, ?, ?)`,
		tenantID, name, isAdmin,
	)
	if err != nil {
		t.Fatalf("insert group %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("group last id: %v", err)
	}
	return data.GroupID(id)
}

func insertMembership(t *testing.T, central *sql.DB, user data.UserID, group data.GroupID) {
	t.Helper()
	if _, err := central.Exec(
		`INSERT INTO "GROUP_MEMBERSHIP" (user_id, group_id) VALUES (?, ?)`,
		int64(user), int64(group),
	); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
}

// insertSession inserts a SESSION row and returns its opaque token. created_at
// and last_seen_at are set to now so the session is within both validity bounds.
func insertSession(t *testing.T, central *sql.DB, user data.UserID, now string) string {
	t.Helper()
	token := recordid.New().Canonical() + recordid.New().Canonical()
	if _, err := central.Exec(
		`INSERT INTO "SESSION" (token, user_id, created_at, last_seen_at) VALUES (?, ?, ?, ?)`,
		token, int64(user), now, now,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return token
}

// seededSphere carries both id forms so tests can seed a grant (needs the
// surrogate) and assert against the canonical Record_ID.
type seededSphere struct {
	surrogate int64
	canonical string
}

func insertSphere(t *testing.T, tenant *sql.DB, name string) seededSphere {
	t.Helper()
	rid := recordid.New()
	res, err := tenant.Exec(
		`INSERT INTO sphere (record_id, name, created_at) VALUES (?, ?, '2024-01-01T00:00:00Z')`,
		rid.Canonical(), name,
	)
	if err != nil {
		t.Fatalf("insert sphere %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("sphere last id: %v", err)
	}
	return seededSphere{surrogate: id, canonical: rid.Canonical()}
}

func grantSphere(t *testing.T, tenant *sql.DB, sphereSurrogate int64, group data.GroupID, access string, reveal bool) {
	t.Helper()
	rev := 0
	if reveal {
		rev = 1
	}
	if _, err := tenant.Exec(
		`INSERT INTO sphere_grant (sphere_id, group_id, access, reveal) VALUES (?, ?, ?, ?)`,
		sphereSurrogate, int64(group), access, rev,
	); err != nil {
		t.Fatalf("grant sphere: %v", err)
	}
}
