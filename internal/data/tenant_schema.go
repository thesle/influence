package data

import (
	"context"
	"database/sql"
	"fmt"
)

// This file owns the Tenant_Database schema and migrations (task 3.2). Each
// Tenant_Database is one self-contained SQLite file holding everything for a
// single tenant — content, image blobs, ciphertext, and the key material to
// decrypt it — with no dependency on an external store and no other tenant's
// data mixed in (Requirements 2.1, 2.3).
//
// The Central_Directory schema (task 3.1) lives in a separate file/namespace;
// nothing here touches identity/registry tables, so the two migration sets can
// evolve in parallel without collision.

// TenantSchemaVersion is the current version of the Tenant_Database schema.
// MigrateTenant applies every step up to this version. Bump it and append a
// migration step when the tenant schema changes.
const TenantSchemaVersion = 1

// tenantMigration is one ordered, idempotent step in the Tenant_Database
// migration chain. Steps run inside a single transaction in ascending version
// order; only steps newer than the database's recorded user_version run.
type tenantMigration struct {
	version int
	name    string
	stmts   []string
}

// tenantMigrations is the ordered Tenant_Database migration chain. Every UI
// record type (Sphere, Facet, Polygon) carries a UNIQUE record_id — the
// external, immutable identifier (Requirements 6.3, 8.3, 10.4, 15.4) — while
// integer surrogate id columns serve as efficient internal foreign keys.
var tenantMigrations = []tenantMigration{
	{
		version: 1,
		name:    "initial tenant schema",
		stmts: []string{
			// SPHERE — top-level workspace. record_id is the immutable external
			// identifier (Req 6.3, 15.4); name is 1-100 chars, unique within the
			// tenant (uniqueness/length enforced in the service layer, task 8.1).
			`CREATE TABLE sphere (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				record_id  TEXT NOT NULL UNIQUE,
				name       TEXT NOT NULL,
				created_at TEXT NOT NULL
			)`,

			// FACET — hierarchical category hub within a Sphere. parent_facet_id
			// is nullable and, when set, must reference a Facet in the same Sphere
			// (enforced in the service layer, task 9.2). record_id is the
			// immutable external identifier (Req 8.3, 15.4).
			`CREATE TABLE facet (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				record_id       TEXT NOT NULL UNIQUE,
				sphere_id       INTEGER NOT NULL REFERENCES sphere(id) ON DELETE CASCADE,
				parent_facet_id INTEGER REFERENCES facet(id) ON DELETE CASCADE,
				name            TEXT NOT NULL
			)`,

			// POLYGON — content unit, one of four types. record_id is unique
			// within the tenant (Req 10.4, 15.4). facet_id and parent_polygon_id
			// are nullable; parent_polygon_id is only set for Folder_Polygon
			// containment. author_user_id references a Central_Directory user id.
			`CREATE TABLE polygon (
				id                INTEGER PRIMARY KEY AUTOINCREMENT,
				record_id         TEXT NOT NULL UNIQUE,
				sphere_id         INTEGER NOT NULL REFERENCES sphere(id) ON DELETE CASCADE,
				facet_id          INTEGER REFERENCES facet(id) ON DELETE SET NULL,
				parent_polygon_id INTEGER REFERENCES polygon(id) ON DELETE CASCADE,
				type              TEXT NOT NULL,
				content           TEXT NOT NULL DEFAULT '',
				edit_mode         TEXT NOT NULL DEFAULT 'locking',
				author_user_id    INTEGER NOT NULL,
				created_at        TEXT NOT NULL,
				updated_at        TEXT NOT NULL
			)`,

			// CIPHERTEXT — one row per obfuscation token. token_id is the {id} in
			// the inline |encrypt|{id}| token; ciphertext is AES-256-GCM with its
			// own random nonce (Req 16.1, 16.2).
			`CREATE TABLE ciphertext (
				token_id   TEXT PRIMARY KEY,
				polygon_id INTEGER NOT NULL REFERENCES polygon(id) ON DELETE CASCADE,
				nonce      BLOB NOT NULL,
				ciphertext BLOB NOT NULL
			)`,

			// IMAGE_BLOB — pasted images stored inline in the tenant file so the
			// export is self-contained (Req 2.1, 17.1). Size (<=10MB) and MIME
			// validation happen in the service layer (task 14.1).
			`CREATE TABLE image_blob (
				image_id TEXT PRIMARY KEY,
				data     BLOB NOT NULL,
				mime     TEXT NOT NULL
			)`,

			// SPHERE_GRANT — per-Sphere access map. group_id references a
			// Central_Directory group id; access is read|write and reveal is a
			// per-Sphere flag (Req 4.3, 16.6). Kept in the tenant file so an
			// exported tenant carries its own access map.
			`CREATE TABLE sphere_grant (
				sphere_id INTEGER NOT NULL REFERENCES sphere(id) ON DELETE CASCADE,
				group_id  INTEGER NOT NULL,
				access    TEXT NOT NULL,
				reveal    INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (sphere_id, group_id)
			)`,

			// TENANT_SECRET — the Tenant_Salt and wrapped key material that make
			// ciphertext decryptable after export (Req 2.2, 16.1, 16.9).
			`CREATE TABLE tenant_secret (
				id          INTEGER PRIMARY KEY CHECK (id = 1),
				tenant_salt BLOB NOT NULL,
				wrapped_key BLOB NOT NULL
			)`,

			// EDIT_LOCK — record-locking state, one row per locked Polygon.
			// last_activity_at drives the 300s inactivity timeout (Req 25.5).
			`CREATE TABLE edit_lock (
				polygon_id       INTEGER PRIMARY KEY REFERENCES polygon(id) ON DELETE CASCADE,
				holder_user_id   INTEGER NOT NULL,
				acquired_at      TEXT NOT NULL,
				last_activity_at TEXT NOT NULL
			)`,

			// USER_CIRCLE — per-user pinned/ordered Spheres (Req 7.4). user_id is
			// a Central_Directory user id; position is the circled order.
			`CREATE TABLE user_circle (
				user_id   INTEGER NOT NULL,
				sphere_id INTEGER NOT NULL REFERENCES sphere(id) ON DELETE CASCADE,
				position  INTEGER NOT NULL,
				PRIMARY KEY (user_id, sphere_id)
			)`,

			// BOOKMARK — per-user bookmarked Polygons (Req 26). user_id is a
			// Central_Directory user id.
			`CREATE TABLE bookmark (
				user_id    INTEGER NOT NULL,
				polygon_id INTEGER NOT NULL REFERENCES polygon(id) ON DELETE CASCADE,
				PRIMARY KEY (user_id, polygon_id)
			)`,

			// VIEW_LOG — per-user view history backing "Recently Viewed
			// Polygons" (Req 24.2). user_id is a Central_Directory user id.
			`CREATE TABLE view_log (
				user_id    INTEGER NOT NULL,
				polygon_id INTEGER NOT NULL REFERENCES polygon(id) ON DELETE CASCADE,
				viewed_at  TEXT NOT NULL
			)`,

			// polygon_fts — FTS5 virtual table indexing MASKED content only, so
			// plaintext and ciphertext never enter the index (Req 16.7, 23.2).
			// record_id is stored UNINDEXED to map hits back to Polygons.
			`CREATE VIRTUAL TABLE polygon_fts USING fts5(
				record_id UNINDEXED,
				content
			)`,

			// Indexes for the common lookups: records by external id, children by
			// parent, and view history newest-first.
			`CREATE INDEX idx_facet_sphere ON facet(sphere_id)`,
			`CREATE INDEX idx_facet_parent ON facet(parent_facet_id)`,
			`CREATE INDEX idx_polygon_sphere ON polygon(sphere_id)`,
			`CREATE INDEX idx_polygon_facet ON polygon(facet_id)`,
			`CREATE INDEX idx_polygon_parent ON polygon(parent_polygon_id)`,
			`CREATE INDEX idx_polygon_author ON polygon(author_user_id)`,
			`CREATE INDEX idx_ciphertext_polygon ON ciphertext(polygon_id)`,
			`CREATE INDEX idx_view_log_user ON view_log(user_id, viewed_at)`,
		},
	},
}

// MigrateTenant brings a Tenant_Database up to TenantSchemaVersion. It is
// idempotent: it reads the database's user_version and applies only the
// migration steps that have not yet run, all inside a single transaction so a
// failed migration leaves the file untouched.
//
// Foreign keys are enabled on the connection before migrating so the
// ON DELETE CASCADE rules (Sphere -> Facet/Polygon, etc.) are enforced.
func MigrateTenant(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}

	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current >= TenantSchemaVersion {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, m := range tenantMigrations {
		if m.version <= current {
			continue
		}
		for _, stmt := range m.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("tenant migration %d (%s): %w", m.version, m.name, err)
			}
		}
	}

	// PRAGMA user_version does not accept a bound parameter, so the target
	// version (a trusted internal constant) is formatted directly.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", TenantSchemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tenant migration: %w", err)
	}
	return nil
}
