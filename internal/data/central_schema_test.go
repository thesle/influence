package data

import (
	"context"
	"database/sql"
	"sort"
	"testing"

	"pgregory.net/rapid"
)

// openMemoryCentral opens a fresh in-memory Central_Directory with foreign keys
// enabled and migrates it to the latest schema.
func openMemoryCentral(t *testing.T) *sql.DB {
	t.Helper()
	// A shared-cache in-memory DB keyed by the test name keeps a single logical
	// database across the pool's connections for the life of the test.
	db, err := sql.Open(DriverName, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	// Pin one connection so the shared-cache in-memory DB is not torn down.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if err := MigrateCentral(context.Background(), db); err != nil {
		t.Fatalf("migrate central: %v", err)
	}
	return db
}

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	sort.Strings(names)
	return names
}

// TestMigrateCentralCreatesTables asserts the migration creates exactly the
// identity/registry tables and no content tables (Requirements 1.1, 1.2).
func TestMigrateCentralCreatesTables(t *testing.T) {
	db := openMemoryCentral(t)

	got := tableNames(t, db)
	want := []string{"GROUP", "GROUP_MEMBERSHIP", "RECOVERY_CODE", "SESSION", "TENANT", "TWO_FACTOR_POLICY", "USER"}
	if len(got) != len(want) {
		t.Fatalf("table set mismatch:\n got=%v\nwant=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("table set mismatch:\n got=%v\nwant=%v", got, want)
		}
	}

	// Guard against content tables leaking into the Central_Directory (Req 1.2).
	forbidden := []string{"SPHERE", "FACET", "POLYGON", "CIPHERTEXT", "IMAGE_BLOB"}
	for _, name := range got {
		for _, bad := range forbidden {
			if name == bad {
				t.Fatalf("content table %q must not exist in Central_Directory (Req 1.2)", bad)
			}
		}
	}
}

// TestMigrateCentralIdempotent asserts running the migration twice is a no-op
// and leaves the schema version stable.
func TestMigrateCentralIdempotent(t *testing.T) {
	db := openMemoryCentral(t)
	ctx := context.Background()

	v1, err := centralSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if err := MigrateCentral(ctx, db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	v2, err := centralSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v1 != v2 {
		t.Fatalf("schema version changed on re-migrate: %d -> %d", v1, v2)
	}
	if v1 != len(centralMigrations) {
		t.Fatalf("expected version %d, got %d", len(centralMigrations), v1)
	}
}

// seedTenant inserts a tenant and returns its id.
func seedTenant(t *testing.T, db *sql.DB, uuid, name string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO "TENANT"(tenant_uuid, name, db_path, status, created_at) VALUES(?,?,?,?,?)`,
		uuid, name, "/var/lib/influence/"+uuid+".db", "pending", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// TestUserSchemaColumns verifies the USER table carries the auth/security
// columns the design requires (Argon2id hash, lockout state, deactivation).
func TestUserSchemaColumns(t *testing.T) {
	db := openMemoryCentral(t)
	tenantID := seedTenant(t, db, "uuid-a", "Org A")

	// Insert a user relying on defaults for the security columns.
	_, err := db.Exec(
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?,?,?,?,?)`,
		tenantID, "alice", "Alice", "argon2id$hash", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var failed, deactivated int
	var lockedUntil sql.NullString
	err = db.QueryRow(
		`SELECT failed_attempts, locked_until, deactivated FROM "USER" WHERE username='alice'`,
	).Scan(&failed, &lockedUntil, &deactivated)
	if err != nil {
		t.Fatalf("select user: %v", err)
	}
	if failed != 0 {
		t.Fatalf("failed_attempts default = %d, want 0", failed)
	}
	if lockedUntil.Valid {
		t.Fatalf("locked_until should default to NULL, got %q", lockedUntil.String)
	}
	if deactivated != 0 {
		t.Fatalf("deactivated default = %d, want 0", deactivated)
	}
}

// TestUsernameUniquePerTenant asserts usernames are unique within a tenant but
// may repeat across tenants.
func TestUsernameUniquePerTenant(t *testing.T) {
	db := openMemoryCentral(t)
	a := seedTenant(t, db, "uuid-a", "Org A")
	b := seedTenant(t, db, "uuid-b", "Org B")

	insert := func(tenant int64, user string) error {
		_, err := db.Exec(
			`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
			 VALUES(?,?,?,?,?)`,
			tenant, user, user, "hash", "2024-01-01T00:00:00Z",
		)
		return err
	}

	if err := insert(a, "bob"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Same username, different tenant: allowed.
	if err := insert(b, "bob"); err != nil {
		t.Fatalf("cross-tenant duplicate should be allowed: %v", err)
	}
	// Same username, same tenant: rejected.
	if err := insert(a, "bob"); err == nil {
		t.Fatal("duplicate username within tenant should be rejected")
	}
}

// TestForeignKeysEnforced asserts referential integrity holds: a session cannot
// reference a nonexistent user, and cascade delete removes dependents.
func TestForeignKeysEnforced(t *testing.T) {
	db := openMemoryCentral(t)
	tenantID := seedTenant(t, db, "uuid-a", "Org A")

	res, err := db.Exec(
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?,?,?,?,?)`,
		tenantID, "carol", "Carol", "hash", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	userID, _ := res.LastInsertId()

	// Session referencing a missing user must be rejected.
	if _, err := db.Exec(
		`INSERT INTO "SESSION"(token, user_id, created_at, last_seen_at) VALUES(?,?,?,?)`,
		"tok1", userID+9999, "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z",
	); err == nil {
		t.Fatal("session with dangling user_id should be rejected by FK")
	}

	// Valid session, then cascade delete via the tenant.
	if _, err := db.Exec(
		`INSERT INTO "SESSION"(token, user_id, created_at, last_seen_at) VALUES(?,?,?,?)`,
		"tok2", userID, "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM "TENANT" WHERE id=?`, tenantID); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	var sessions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM "SESSION"`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Fatalf("expected cascade delete to remove sessions, %d remain", sessions)
	}
}

// TestTenantStatusCheck asserts the status CHECK constraint rejects values
// outside the pending|active set.
func TestTenantStatusCheck(t *testing.T) {
	db := openMemoryCentral(t)
	if _, err := db.Exec(
		`INSERT INTO "TENANT"(tenant_uuid, name, db_path, status, created_at) VALUES(?,?,?,?,?)`,
		"uuid-bad", "Bad", "/tmp/x.db", "archived", "2024-01-01T00:00:00Z",
	); err == nil {
		t.Fatal("invalid tenant status should be rejected by CHECK constraint")
	}
}

// TestGroupMembershipUnique is a property test asserting the composite primary
// key on GROUP_MEMBERSHIP makes membership insertion idempotency-safe: a
// duplicate (user_id, group_id) pair is always rejected regardless of how many
// distinct valid pairs precede it. This anchors the storage-layer guarantee
// behind the >=1-membership rule (Req 4.1).
func TestGroupMembershipUnique(t *testing.T) {
	db := openMemoryCentral(t)
	tenantID := seedTenant(t, db, "uuid-a", "Org A")

	// One user and one group to form a single membership edge.
	ures, err := db.Exec(
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?,?,?,?,?)`, tenantID, "dave", "Dave", "hash", "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	userID, _ := ures.LastInsertId()

	gres, err := db.Exec(
		`INSERT INTO "GROUP"(tenant_id, name, is_admin) VALUES(?,?,?)`,
		tenantID, "Editors", 0)
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	groupID, _ := gres.LastInsertId()

	rapid.Check(t, func(rt *rapid.T) {
		// Number of times we try to insert the SAME edge (>=1).
		attempts := rapid.IntRange(1, 5).Draw(rt, "attempts")

		var successes int
		for i := 0; i < attempts; i++ {
			_, err := db.Exec(
				`INSERT INTO "GROUP_MEMBERSHIP"(user_id, group_id) VALUES(?,?)`,
				userID, groupID)
			if err == nil {
				successes++
			}
		}
		// Exactly one insert can ever succeed for a given edge; the rest are
		// rejected by the composite primary key.
		if successes > 1 {
			rt.Fatalf("duplicate membership edge inserted %d times, want at most 1", successes)
		}
		// Reset for the next iteration so edges don't accumulate.
		if _, err := db.Exec(`DELETE FROM "GROUP_MEMBERSHIP" WHERE user_id=? AND group_id=?`, userID, groupID); err != nil {
			rt.Fatalf("cleanup membership: %v", err)
		}
	})
}

// TestTwoFactorColumnDefaults asserts the additive 2FA columns on USER default
// so a user inserted without them migrates/behaves cleanly: the secret and
// last-step are NULL and enrolment is 0 (Req 33.1, 33.2, 33.9).
func TestTwoFactorColumnDefaults(t *testing.T) {
	db := openMemoryCentral(t)
	tenantID := seedTenant(t, db, "uuid-a", "Org A")

	_, err := db.Exec(
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?,?,?,?,?)`,
		tenantID, "erin", "Erin", "hash", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var secret sql.NullString
	var enrolled int
	var lastStep sql.NullInt64
	err = db.QueryRow(
		`SELECT two_factor_secret, two_factor_enrolled, last_totp_step FROM "USER" WHERE username='erin'`,
	).Scan(&secret, &enrolled, &lastStep)
	if err != nil {
		t.Fatalf("select user 2fa columns: %v", err)
	}
	if secret.Valid {
		t.Fatalf("two_factor_secret should default to NULL, got %q", secret.String)
	}
	if enrolled != 0 {
		t.Fatalf("two_factor_enrolled default = %d, want 0", enrolled)
	}
	if lastStep.Valid {
		t.Fatalf("last_totp_step should default to NULL, got %d", lastStep.Int64)
	}
}

// TestSessionStateDefault asserts SESSION.state defaults to 'active' so
// pre-existing (pre-migration) sessions remain fully authenticated (Req 33.4),
// and that the CHECK constraint rejects an out-of-set state.
func TestSessionStateDefault(t *testing.T) {
	db := openMemoryCentral(t)
	tenantID := seedTenant(t, db, "uuid-a", "Org A")
	res, err := db.Exec(
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?,?,?,?,?)`,
		tenantID, "frank", "Frank", "hash", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	userID, _ := res.LastInsertId()

	// Insert relying on the default state.
	if _, err := db.Exec(
		`INSERT INTO "SESSION"(token, user_id, created_at, last_seen_at) VALUES(?,?,?,?)`,
		"tok-default", userID, "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM "SESSION" WHERE token='tok-default'`).Scan(&state); err != nil {
		t.Fatalf("select session state: %v", err)
	}
	if state != "active" {
		t.Fatalf("session state default = %q, want \"active\"", state)
	}

	// Each valid state must be accepted.
	for i, s := range []string{"active", "pending_2fa", "must_enrol"} {
		if _, err := db.Exec(
			`INSERT INTO "SESSION"(token, user_id, created_at, last_seen_at, state) VALUES(?,?,?,?,?)`,
			"tok-valid-"+s, userID, "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z", s,
		); err != nil {
			t.Fatalf("valid state %q (case %d) rejected: %v", s, i, err)
		}
	}

	// An out-of-set state must be rejected by the CHECK constraint.
	if _, err := db.Exec(
		`INSERT INTO "SESSION"(token, user_id, created_at, last_seen_at, state) VALUES(?,?,?,?,?)`,
		"tok-bad", userID, "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z", "half",
	); err == nil {
		t.Fatal("invalid session state should be rejected by CHECK constraint")
	}
}

// TestRecoveryCodeSchema asserts RECOVERY_CODE defaults used=0, enforces its
// used CHECK, and cascades on user deletion (Req 33.2, 33.6).
func TestRecoveryCodeSchema(t *testing.T) {
	db := openMemoryCentral(t)
	tenantID := seedTenant(t, db, "uuid-a", "Org A")
	res, err := db.Exec(
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?,?,?,?,?)`,
		tenantID, "grace", "Grace", "hash", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	userID, _ := res.LastInsertId()

	if _, err := db.Exec(
		`INSERT INTO "RECOVERY_CODE"(user_id, code_hash) VALUES(?,?)`,
		userID, "argon2id$code",
	); err != nil {
		t.Fatalf("insert recovery code: %v", err)
	}
	var used int
	if err := db.QueryRow(`SELECT used FROM "RECOVERY_CODE" WHERE user_id=?`, userID).Scan(&used); err != nil {
		t.Fatalf("select recovery code: %v", err)
	}
	if used != 0 {
		t.Fatalf("recovery code used default = %d, want 0", used)
	}

	// used outside {0,1} is rejected.
	if _, err := db.Exec(
		`INSERT INTO "RECOVERY_CODE"(user_id, code_hash, used) VALUES(?,?,?)`,
		userID, "argon2id$code2", 2,
	); err == nil {
		t.Fatal("recovery code used=2 should be rejected by CHECK constraint")
	}

	// A dangling user_id is rejected by the FK.
	if _, err := db.Exec(
		`INSERT INTO "RECOVERY_CODE"(user_id, code_hash) VALUES(?,?)`,
		userID+9999, "argon2id$orphan",
	); err == nil {
		t.Fatal("recovery code with dangling user_id should be rejected by FK")
	}

	// Deleting the user cascades to their recovery codes.
	if _, err := db.Exec(`DELETE FROM "USER" WHERE id=?`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM "RECOVERY_CODE"`).Scan(&remaining); err != nil {
		t.Fatalf("count recovery codes: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected cascade delete to remove recovery codes, %d remain", remaining)
	}
}

// TestTwoFactorPolicySchema asserts TWO_FACTOR_POLICY defaults required=0, is
// at most one row per tenant (tenant_id PK), enforces its CHECK, and cascades
// on tenant deletion (Req 33.7, 33.8).
func TestTwoFactorPolicySchema(t *testing.T) {
	db := openMemoryCentral(t)
	tenantID := seedTenant(t, db, "uuid-a", "Org A")

	if _, err := db.Exec(`INSERT INTO "TWO_FACTOR_POLICY"(tenant_id) VALUES(?)`, tenantID); err != nil {
		t.Fatalf("insert policy: %v", err)
	}
	var required int
	if err := db.QueryRow(`SELECT required FROM "TWO_FACTOR_POLICY" WHERE tenant_id=?`, tenantID).Scan(&required); err != nil {
		t.Fatalf("select policy: %v", err)
	}
	if required != 0 {
		t.Fatalf("policy required default = %d, want 0", required)
	}

	// A second row for the same tenant is rejected by the primary key.
	if _, err := db.Exec(`INSERT INTO "TWO_FACTOR_POLICY"(tenant_id, required) VALUES(?,?)`, tenantID, 1); err == nil {
		t.Fatal("duplicate policy row for a tenant should be rejected by PK")
	}

	// required outside {0,1} is rejected.
	other := seedTenant(t, db, "uuid-b", "Org B")
	if _, err := db.Exec(`INSERT INTO "TWO_FACTOR_POLICY"(tenant_id, required) VALUES(?,?)`, other, 5); err == nil {
		t.Fatal("policy required=5 should be rejected by CHECK constraint")
	}

	// Deleting the tenant cascades to its policy.
	if _, err := db.Exec(`DELETE FROM "TENANT" WHERE id=?`, tenantID); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM "TWO_FACTOR_POLICY"`).Scan(&remaining); err != nil {
		t.Fatalf("count policies: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected cascade delete to remove policy, %d remain", remaining)
	}
}

// TestTwoFactorMigrationBackwardCompatible asserts the 2FA migration (version
// 2) applies cleanly on top of a version-1 Central_Directory that already
// holds data, without disturbing existing rows. This exercises the
// backward-compatibility requirement directly (Req 33 additive migration): we
// build a v1 schema, seed it, then run the full migration and confirm the seed
// survives and picks up the defaulted 2FA state.
func TestTwoFactorMigrationBackwardCompatible(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(DriverName, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// Apply only the version-1 migration to simulate a pre-2FA database.
	var v1 centralMigration
	for _, m := range centralMigrations {
		if m.version == 1 {
			v1 = m
		}
	}
	if v1.version != 1 {
		t.Fatal("version-1 migration not found")
	}
	if err := applyCentralMigration(ctx, db, v1); err != nil {
		t.Fatalf("apply v1 migration: %v", err)
	}

	// Seed a tenant, a user, and a session under the v1 schema.
	tenantID := seedTenant(t, db, "uuid-a", "Org A")
	res, err := db.Exec(
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?,?,?,?,?)`,
		tenantID, "heidi", "Heidi", "hash", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	userID, _ := res.LastInsertId()
	if _, err := db.Exec(
		`INSERT INTO "SESSION"(token, user_id, created_at, last_seen_at) VALUES(?,?,?,?)`,
		"legacy-tok", userID, "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Now run the full migration; it must advance to the latest version.
	if err := MigrateCentral(ctx, db); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}
	v, err := centralSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != len(centralMigrations) {
		t.Fatalf("post-migration version = %d, want %d", v, len(centralMigrations))
	}

	// The pre-existing user survived and picked up the defaulted 2FA state.
	var enrolled int
	var secret sql.NullString
	if err := db.QueryRow(
		`SELECT two_factor_enrolled, two_factor_secret FROM "USER" WHERE username='heidi'`,
	).Scan(&enrolled, &secret); err != nil {
		t.Fatalf("select migrated user: %v", err)
	}
	if enrolled != 0 || secret.Valid {
		t.Fatalf("migrated user should be un-enrolled with NULL secret, got enrolled=%d secretValid=%v", enrolled, secret.Valid)
	}

	// The pre-existing session survived and defaulted to 'active'.
	var state string
	if err := db.QueryRow(`SELECT state FROM "SESSION" WHERE token='legacy-tok'`).Scan(&state); err != nil {
		t.Fatalf("select migrated session: %v", err)
	}
	if state != "active" {
		t.Fatalf("migrated session state = %q, want \"active\"", state)
	}
}
