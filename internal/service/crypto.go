package service

// Tenant key material and derivation (design § Key derivation and cipher,
// Requirements 16.1, 16.9, 2.2).
//
// Each tenant owns a random 128-bit Tenant_Salt and a random 256-bit Data
// Encryption Key (DEK). The DEK is what actually encrypts sensitive spans with
// AES-256-GCM (tasks 12.2/12.4); it is generated once at tenant creation and
// never stored in the clear. Instead it is wrapped (encrypted) under a
// Key-Encryption-Key (KEK) that is derived on demand via Argon2id from the
// server master secret salted with the tenant's Tenant_Salt. The Tenant_Salt
// and the wrapped DEK both live in the tenant DB's single TENANT_SECRET row
// (id = 1).
//
// Keeping both the salt and the wrapped key inside the tenant file is what
// makes an exported tenant decryptable on a successor host that holds the same
// server master secret (Req 2.2, 16.9): the file carries everything except the
// master secret needed to re-derive the KEK and unwrap the DEK.
//
// Wrapping uses AES-256-GCM: the KEK is the 256-bit AES key, a fresh random
// 96-bit nonce is prepended to the ciphertext, and the GCM tag authenticates
// the wrapped bytes. A wrong master secret derives a different KEK, so the GCM
// authentication fails and UnwrapDEK returns an error rather than silently
// yielding a wrong key.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

const (
	// tenantSaltLen is the length in bytes of the per-tenant Tenant_Salt
	// (128 bits, Req 16.1).
	tenantSaltLen = 16
	// dekLen is the length in bytes of the Data Encryption Key (256 bits, so
	// the DEK itself is a valid AES-256 key for span encryption).
	dekLen = 32
	// kekLen is the length in bytes of the Key-Encryption-Key derived via
	// Argon2id (256 bits, for AES-256-GCM wrapping).
	kekLen = 32
)

// KEK-derivation cost parameters for Argon2id. These mirror the interactive
// password-hashing defaults (memory-hard, current OWASP guidance) and are fixed
// for the KEK because, unlike a stored password hash, the KEK is re-derived
// from the same inputs every time it is needed rather than parsed from an
// encoded string.
const (
	kekArgonTime    = 3
	kekArgonMemory  = 64 * 1024
	kekArgonThreads = 2
)

// ErrTenantSecretExists is returned by InitTenantSecret when the tenant DB
// already has a TENANT_SECRET row, so re-initialising would replace live key
// material and orphan every existing ciphertext.
var ErrTenantSecretExists = errors.New("tenant secret already initialized")

// ErrTenantSecretMissing is returned by UnwrapDEK when the tenant DB has no
// TENANT_SECRET row to unwrap.
var ErrTenantSecretMissing = errors.New("tenant secret not initialized")

// ErrUnwrapFailed is returned when the wrapped DEK cannot be unwrapped — either
// the stored material is corrupt or the master secret is wrong, so the derived
// KEK does not authenticate the ciphertext. It never exposes which of these is
// the case.
var ErrUnwrapFailed = errors.New("unwrap DEK failed")

// InitTenantSecret provisions the key material for a freshly created tenant. It
// generates a random 128-bit Tenant_Salt and a random 256-bit DEK, derives a
// KEK from masterSecret salted with the Tenant_Salt, wraps the DEK under the
// KEK with AES-256-GCM, and stores the Tenant_Salt plus wrapped DEK in the
// tenant DB's TENANT_SECRET row (id = 1). It is called once during tenant
// creation, before any span is encrypted (Req 16.1).
//
// It refuses to overwrite existing key material: if TENANT_SECRET is already
// populated it returns ErrTenantSecretExists and writes nothing.
func InitTenantSecret(ctx context.Context, tenantDB *sql.DB, masterSecret string) error {
	var existing int
	err := tenantDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM tenant_secret").Scan(&existing)
	if err != nil {
		return fmt.Errorf("check tenant secret: %w", err)
	}
	if existing > 0 {
		return ErrTenantSecretExists
	}

	salt := make([]byte, tenantSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate tenant salt: %w", err)
	}

	dek := make([]byte, dekLen)
	if _, err := rand.Read(dek); err != nil {
		return fmt.Errorf("generate dek: %w", err)
	}

	wrapped, err := wrapDEK(masterSecret, salt, dek)
	if err != nil {
		return fmt.Errorf("wrap dek: %w", err)
	}

	_, err = tenantDB.ExecContext(ctx,
		"INSERT INTO tenant_secret (id, tenant_salt, wrapped_key) VALUES (1, ?, ?)",
		salt, wrapped,
	)
	if err != nil {
		return fmt.Errorf("store tenant secret: %w", err)
	}
	return nil
}

// UnwrapDEK reads the tenant's Tenant_Salt and wrapped DEK from TENANT_SECRET,
// re-derives the KEK from masterSecret salted with the Tenant_Salt, and unwraps
// the DEK. It returns the 256-bit DEK used to encrypt and reveal sensitive
// spans (tasks 12.2, 12.4) and to decrypt an exported tenant on a successor
// host (Req 16.9, 2.2).
//
// If TENANT_SECRET has no row it returns ErrTenantSecretMissing. If the master
// secret is wrong or the stored material is corrupt, GCM authentication fails
// and it returns ErrUnwrapFailed without yielding any key bytes.
func UnwrapDEK(ctx context.Context, tenantDB *sql.DB, masterSecret string) ([]byte, error) {
	var salt, wrapped []byte
	err := tenantDB.QueryRowContext(ctx,
		"SELECT tenant_salt, wrapped_key FROM tenant_secret WHERE id = 1",
	).Scan(&salt, &wrapped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantSecretMissing
	}
	if err != nil {
		return nil, fmt.Errorf("read tenant secret: %w", err)
	}

	dek, err := unwrapDEK(masterSecret, salt, wrapped)
	if err != nil {
		return nil, err
	}
	return dek, nil
}

// deriveKEK derives the 256-bit Key-Encryption-Key via Argon2id from the server
// master secret salted with the tenant's Tenant_Salt (design § Key derivation
// and cipher). The same inputs always yield the same KEK, so it can be
// re-derived on demand to unwrap the DEK.
func deriveKEK(masterSecret string, salt []byte) []byte {
	return argon2.IDKey([]byte(masterSecret), salt, kekArgonTime, kekArgonMemory, kekArgonThreads, kekLen)
}

// wrapDEK encrypts the DEK under a KEK derived from masterSecret and salt using
// AES-256-GCM, returning nonce||ciphertext. The random 96-bit nonce is
// prepended so unwrapDEK can recover it.
func wrapDEK(masterSecret string, salt, dek []byte) ([]byte, error) {
	gcm, err := newKEKGCM(masterSecret, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	// Seal appends the ciphertext+tag to nonce, producing nonce||ct.
	return gcm.Seal(nonce, nonce, dek, nil), nil
}

// unwrapDEK reverses wrapDEK: it re-derives the KEK and AES-256-GCM opens
// nonce||ciphertext. A wrong master secret or corrupt material fails GCM
// authentication and yields ErrUnwrapFailed.
func unwrapDEK(masterSecret string, salt, wrapped []byte) ([]byte, error) {
	gcm, err := newKEKGCM(masterSecret, salt)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < gcm.NonceSize() {
		return nil, ErrUnwrapFailed
	}
	nonce := wrapped[:gcm.NonceSize()]
	ct := wrapped[gcm.NonceSize():]
	dek, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, ErrUnwrapFailed
	}
	return dek, nil
}

// newKEKGCM derives the KEK and returns an AES-256-GCM AEAD keyed with it.
func newKEKGCM(masterSecret string, salt []byte) (cipher.AEAD, error) {
	kek := deriveKEK(masterSecret, salt)
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return gcm, nil
}
