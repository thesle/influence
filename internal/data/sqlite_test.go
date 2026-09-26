package data

import (
	"database/sql"
	"testing"

	"pgregory.net/rapid"
)

// TestDriverNameStable anchors pgregory.net/rapid as a test dependency (task
// 1.1). The FTS5-availability smoke test lands in task 1.2; this trivial
// property just confirms the constant is stable across the property's inputs.
func TestDriverNameStable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		_ = rapid.IntRange(0, 10).Draw(t, "n")
		if DriverName != "sqlite" {
			t.Fatalf("expected driver name %q, got %q", "sqlite", DriverName)
		}
	})
}

// TestFTS5Available is a smoke test for task 1.2. Influence stores everything in
// SQLite with no separate database server (Requirement 20.3) and its search
// relies on SQLite FTS5 (Requirement 23.1), so the SQLite driver must be built
// with FTS5 support. This guards the driver/build-tag choice: if FTS5 is ever
// missing, creating the virtual table fails and this test catches it before the
// search feature would break at runtime.
func TestFTS5Available(t *testing.T) {
	db, err := sql.Open(DriverName, ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("ping in-memory sqlite: %v", err)
	}

	if _, err := db.Exec(`CREATE VIRTUAL TABLE fts5_smoke USING fts5(content)`); err != nil {
		t.Fatalf("create FTS5 virtual table (FTS5 unavailable in driver build?): %v", err)
	}

	// Exercise the table to confirm FTS5 is functional, not just parseable.
	if _, err := db.Exec(`INSERT INTO fts5_smoke(content) VALUES ('influence platform search')`); err != nil {
		t.Fatalf("insert into FTS5 table: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM fts5_smoke WHERE fts5_smoke MATCH 'platform'`).Scan(&count); err != nil {
		t.Fatalf("query FTS5 MATCH: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 FTS5 match, got %d", count)
	}
}
