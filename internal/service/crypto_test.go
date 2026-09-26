package service

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/influence/influence/internal/data"
)

// newCryptoTenantDB opens a fresh in-memory Tenant_Database with the tenant
// schema migrated, so TENANT_SECRET exists for InitTenantSecret/UnwrapDEK to
// operate on. A single connection keeps the in-memory database alive.
func newCryptoTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(data.DriverName, ":memory:")
	if err != nil {
		t.Fatalf("open tenant db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := data.MigrateTenant(context.Background(), db); err != nil {
		t.Fatalf("migrate tenant: %v", err)
	}
	return db
}

// TestInitThenUnwrapRoundTrip verifies the core round-trip: after
// InitTenantSecret provisions the key material, UnwrapDEK with the same master
// secret recovers the exact 256-bit DEK, and it is stable across repeated
// unwraps (Req 16.1, 16.9).
func TestInitThenUnwrapRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newCryptoTenantDB(t)
	const master = "server-master-secret"

	if err := InitTenantSecret(ctx, db, master); err != nil {
		t.Fatalf("InitTenantSecret: %v", err)
	}

	dek1, err := UnwrapDEK(ctx, db, master)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if len(dek1) != dekLen {
		t.Fatalf("DEK length = %d, want %d", len(dek1), dekLen)
	}

	dek2, err := UnwrapDEK(ctx, db, master)
	if err != nil {
		t.Fatalf("UnwrapDEK (second): %v", err)
	}
	if !bytes.Equal(dek1, dek2) {
		t.Fatalf("DEK not stable across unwraps: %x vs %x", dek1, dek2)
	}
}

// TestUnwrapWrongMasterSecretFails verifies that a master secret other than the
// one used to wrap the DEK derives a different KEK, so GCM authentication fails
// and no key bytes are returned (Req 16.1).
func TestUnwrapWrongMasterSecretFails(t *testing.T) {
	ctx := context.Background()
	db := newCryptoTenantDB(t)

	if err := InitTenantSecret(ctx, db, "correct-master"); err != nil {
		t.Fatalf("InitTenantSecret: %v", err)
	}

	dek, err := UnwrapDEK(ctx, db, "wrong-master")
	if !errors.Is(err, ErrUnwrapFailed) {
		t.Fatalf("UnwrapDEK with wrong secret error = %v, want ErrUnwrapFailed", err)
	}
	if dek != nil {
		t.Fatalf("expected no DEK on failed unwrap, got %x", dek)
	}
}

// TestInitTenantSecretStoresSaltAndWrappedKey verifies the persisted row: a
// 128-bit Tenant_Salt and a non-empty wrapped key are stored in TENANT_SECRET
// at id = 1, and the wrapped key is not the plaintext DEK (Req 2.2, 16.1).
func TestInitTenantSecretStoresSaltAndWrappedKey(t *testing.T) {
	ctx := context.Background()
	db := newCryptoTenantDB(t)
	const master = "master"

	if err := InitTenantSecret(ctx, db, master); err != nil {
		t.Fatalf("InitTenantSecret: %v", err)
	}

	var salt, wrapped []byte
	err := db.QueryRowContext(ctx,
		"SELECT tenant_salt, wrapped_key FROM tenant_secret WHERE id = 1",
	).Scan(&salt, &wrapped)
	if err != nil {
		t.Fatalf("read tenant_secret: %v", err)
	}
	if len(salt) != tenantSaltLen {
		t.Fatalf("tenant_salt length = %d, want %d", len(salt), tenantSaltLen)
	}
	// wrapped = 96-bit nonce + DEK + GCM tag, so it is strictly larger than the
	// bare DEK and cannot equal it.
	if len(wrapped) <= dekLen {
		t.Fatalf("wrapped_key length = %d, want > %d", len(wrapped), dekLen)
	}

	dek, err := UnwrapDEK(ctx, db, master)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if bytes.Contains(wrapped, dek) {
		t.Fatal("wrapped_key contains the plaintext DEK")
	}
}

// TestInitTenantSecretRefusesOverwrite verifies InitTenantSecret does not
// clobber existing key material: a second call returns ErrTenantSecretExists
// and leaves the original DEK unwrappable.
func TestInitTenantSecretRefusesOverwrite(t *testing.T) {
	ctx := context.Background()
	db := newCryptoTenantDB(t)
	const master = "master"

	if err := InitTenantSecret(ctx, db, master); err != nil {
		t.Fatalf("first InitTenantSecret: %v", err)
	}
	before, err := UnwrapDEK(ctx, db, master)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}

	if err := InitTenantSecret(ctx, db, master); !errors.Is(err, ErrTenantSecretExists) {
		t.Fatalf("second InitTenantSecret error = %v, want ErrTenantSecretExists", err)
	}

	after, err := UnwrapDEK(ctx, db, master)
	if err != nil {
		t.Fatalf("UnwrapDEK after refused overwrite: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("key material changed despite refused overwrite")
	}
}

// TestUnwrapDEKMissingSecret verifies UnwrapDEK reports ErrTenantSecretMissing
// when the tenant has no TENANT_SECRET row yet.
func TestUnwrapDEKMissingSecret(t *testing.T) {
	ctx := context.Background()
	db := newCryptoTenantDB(t)

	dek, err := UnwrapDEK(ctx, db, "master")
	if !errors.Is(err, ErrTenantSecretMissing) {
		t.Fatalf("UnwrapDEK error = %v, want ErrTenantSecretMissing", err)
	}
	if dek != nil {
		t.Fatalf("expected no DEK, got %x", dek)
	}
}
