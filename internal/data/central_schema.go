package data

import (
	"context"
	"database/sql"
	"fmt"
)

// This file defines the Central_Directory schema and its migrations. The
// Central_Directory is the single SQLite database that holds platform-wide
// identity and the tenant registry ONLY — no content or content metadata ever
// lives here (Requirements 1.1, 1.2). Per-tenant content schema is defined
// separately (see the Tenant_Database schema) so the two namespaces never
// collide.
//
// Tables (see the data-model ERD in design.md):
//
//   TENANT            tenant registry (id, tenant_uuid, name, db_path, status)
//   USER              identity + credentials (Argon2id password_hash, brute-force
//                     lockout state, deactivation flag) — Requirements 3.4, 5.4
//   GROUP             group definitions with the Admin_Group flag — Requirement 4.1
//   GROUP_MEMBERSHIP  user↔group membership (a user must have >=1) — Requirement 4.1
//   SESSION           server-side sessions, with a 2FA state — Requirement 3.5, 3.6, 33.4
//   RECOVERY_CODE     single-use 2FA backup codes (Argon2id hashes) — Requirement 33.2, 33.6
//   TWO_FACTOR_POLICY per-tenant optional/required 2FA flag — Requirement 33.7, 33.8
//
// GROUP, USER, and SESSION collide with SQLite keywords, so every table and
// column reference quotes identifiers defensively.

// centralMigration is a single ordered, forward-only schema migration for the
// Central_Directory. Each migration bumps PRAGMA user_version by exactly one.
type centralMigration struct {
	// version is the PRAGMA user_version this migration brings the database to.
	version int
	// stmts are executed in order within a single transaction.
	stmts []string
}

// centralMigrations is the ordered list of Central_Directory migrations. Append
// new migrations here with the next sequential version; never edit or reorder
// an already-released migration.
var centralMigrations = []centralMigration{
	{
		version: 1,
		stmts: []string{
			// Enforce foreign keys for this connection. Set per-connection at
			// migration time; connection setup applies it for normal use too.
			`PRAGMA foreign_keys = ON;`,

			// TENANT — the tenant registry. Holds no content, only where each
			// tenant's isolated database lives and its lifecycle status
			// (pending during creation, active once migrated). Req 1.1, 1.7.
			`CREATE TABLE "TENANT" (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				tenant_uuid TEXT NOT NULL UNIQUE,
				name        TEXT NOT NULL,
				db_path     TEXT NOT NULL,
				status      TEXT NOT NULL DEFAULT 'pending'
				            CHECK (status IN ('pending', 'active')),
				created_at  TEXT NOT NULL
			);`,

			// USER — platform-wide identity and credentials. Each user belongs
			// to exactly one tenant (Req 1.3). password_hash stores a one-way
			// Argon2id hash (Req 3.4). failed_attempts / locked_until back the
			// brute-force lockout (Req 3.3). deactivated denies access while
			// retaining the account (Req 5.4). username is unique per tenant.
			`CREATE TABLE "USER" (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				tenant_id       INTEGER NOT NULL
				                REFERENCES "TENANT"(id) ON DELETE CASCADE,
				username        TEXT NOT NULL,
				display_name    TEXT NOT NULL,
				password_hash   TEXT NOT NULL,
				failed_attempts INTEGER NOT NULL DEFAULT 0,
				locked_until    TEXT,
				deactivated     INTEGER NOT NULL DEFAULT 0
				                CHECK (deactivated IN (0, 1)),
				created_at      TEXT NOT NULL,
				UNIQUE (tenant_id, username)
			);`,
			`CREATE INDEX "idx_user_tenant" ON "USER"(tenant_id);`,

			// GROUP — named collections of users within a tenant. is_admin
			// marks the reserved Admin_Group (Req 4.7, 5). Group names are
			// unique within a tenant.
			`CREATE TABLE "GROUP" (
				id        INTEGER PRIMARY KEY AUTOINCREMENT,
				tenant_id INTEGER NOT NULL
				          REFERENCES "TENANT"(id) ON DELETE CASCADE,
				name      TEXT NOT NULL,
				is_admin  INTEGER NOT NULL DEFAULT 0
				          CHECK (is_admin IN (0, 1)),
				UNIQUE (tenant_id, name)
			);`,
			`CREATE INDEX "idx_group_tenant" ON "GROUP"(tenant_id);`,

			// GROUP_MEMBERSHIP — user↔group edges. A user must belong to at
			// least one group at all times (Req 4.1); that invariant is
			// enforced in the service layer since SQL cannot express a
			// "minimum one row" constraint. The composite PK prevents duplicate
			// memberships.
			`CREATE TABLE "GROUP_MEMBERSHIP" (
				user_id  INTEGER NOT NULL
				         REFERENCES "USER"(id) ON DELETE CASCADE,
				group_id INTEGER NOT NULL
				         REFERENCES "GROUP"(id) ON DELETE CASCADE,
				PRIMARY KEY (user_id, group_id)
			);`,
			`CREATE INDEX "idx_membership_group" ON "GROUP_MEMBERSHIP"(group_id);`,

			// SESSION — server-side session records. token is an opaque
			// high-entropy value (the cookie value). created_at bounds the
			// 24-hour absolute lifetime; last_seen_at backs the 30-minute
			// inactivity slide (Req 3.6). Logout deletes the row (Req 3.5).
			`CREATE TABLE "SESSION" (
				token        TEXT PRIMARY KEY,
				user_id      INTEGER NOT NULL
				             REFERENCES "USER"(id) ON DELETE CASCADE,
				created_at   TEXT NOT NULL,
				last_seen_at TEXT NOT NULL
			);`,
			`CREATE INDEX "idx_session_user" ON "SESSION"(user_id);`,
		},
	},
	{
		// Version 2 — Two-Factor Authentication (Req 33). Every change is
		// additive and defaulted so it migrates an already-populated
		// Central_Directory without touching existing rows: added columns take
		// their DEFAULT for pre-existing rows, and the new tables start empty
		// (an absent TWO_FACTOR_POLICY row means the tenant policy is optional).
		version: 2,
		stmts: []string{
			// USER gains its 2FA state. two_factor_secret holds the pending or
			// active TOTP_Secret (base32), NULL until enrolment begins
			// (Req 33.1). two_factor_enrolled flips to 1 once a TOTP has been
			// confirmed (Req 33.2). last_totp_step records the most recently
			// consumed TOTP time-step so a replayed code inside its window is
			// rejected (Req 33.9); NULL until a code is first consumed.
			`ALTER TABLE "USER" ADD COLUMN two_factor_secret TEXT;`,
			`ALTER TABLE "USER" ADD COLUMN two_factor_enrolled INTEGER NOT NULL DEFAULT 0
			 CHECK (two_factor_enrolled IN (0, 1));`,
			`ALTER TABLE "USER" ADD COLUMN last_totp_step INTEGER;`,

			// RECOVERY_CODE — single-use backup codes issued at enrolment,
			// stored as Argon2id hashes (never in cleartext). used flips to 1
			// on redemption so a code cannot be reused (Req 33.2, 33.6).
			`CREATE TABLE "RECOVERY_CODE" (
				id        INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id   INTEGER NOT NULL
				          REFERENCES "USER"(id) ON DELETE CASCADE,
				code_hash TEXT NOT NULL,
				used      INTEGER NOT NULL DEFAULT 0
				          CHECK (used IN (0, 1))
			);`,
			`CREATE INDEX "idx_recovery_code_user" ON "RECOVERY_CODE"(user_id);`,

			// TWO_FACTOR_POLICY — per-tenant optional/required flag toggled by
			// an admin (Req 33.7, 33.8). The tenant_id is the primary key, so a
			// tenant has at most one policy row; the absence of a row means the
			// policy is optional (the defaulted required=0 matches that).
			`CREATE TABLE "TWO_FACTOR_POLICY" (
				tenant_id INTEGER PRIMARY KEY
				          REFERENCES "TENANT"(id) ON DELETE CASCADE,
				required  INTEGER NOT NULL DEFAULT 0
				          CHECK (required IN (0, 1))
			);`,

			// SESSION gains a state so the middleware can gate a
			// half-authenticated session. 'active' is a normal session;
			// 'pending_2fa' has passed the password but still owes a second
			// factor (Req 33.4); 'must_enrol' is a restricted session that may
			// only reach the enrolment and logout routes (Req 33.7). Existing
			// rows default to 'active', preserving current behaviour.
			`ALTER TABLE "SESSION" ADD COLUMN state TEXT NOT NULL DEFAULT 'active'
			 CHECK (state IN ('active', 'pending_2fa', 'must_enrol'));`,
		},
	},
}

// MigrateCentral brings the Central_Directory database up to the latest schema
// version. It is idempotent: migrations already applied (tracked via
// PRAGMA user_version) are skipped, so calling it on an up-to-date database is
// a no-op. Each pending migration runs in its own transaction; a failure rolls
// that migration back and leaves user_version unchanged.
func MigrateCentral(ctx context.Context, db *sql.DB) error {
	current, err := centralSchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("read central schema version: %w", err)
	}

	for _, m := range centralMigrations {
		if m.version <= current {
			continue
		}
		if err := applyCentralMigration(ctx, db, m); err != nil {
			return fmt.Errorf("apply central migration %d: %w", m.version, err)
		}
		current = m.version
	}
	return nil
}

// applyCentralMigration runs a single migration's statements in one
// transaction and then records the new user_version.
func applyCentralMigration(ctx context.Context, db *sql.DB, m centralMigration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range m.stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("statement failed: %w", err)
		}
	}

	// PRAGMA user_version does not accept bound parameters; the version is an
	// internally controlled integer, so formatting it is safe.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d;", m.version)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return tx.Commit()
}

// centralSchemaVersion reads the current PRAGMA user_version of the
// Central_Directory database. A freshly created database reports 0.
func centralSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version;").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
