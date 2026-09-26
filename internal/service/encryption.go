package service

// Span encryption and obfuscation (design § Encrypt flow, Requirements 16.1,
// 16.2). This is the write side of the encryption feature: it turns a selected
// span of a Polygon's stored content into ciphertext and leaves behind only an
// Obfuscation_Token in its place.
//
// The flow mirrors the design's Encrypt sequence exactly:
//
//   1. Resolve the target Polygon within the caller's own tenant (structural
//      isolation — the tenant handle comes from RequestContext, never client
//      input), reading its current content and owning Sphere.
//   2. Unwrap the tenant's DEK from TENANT_SECRET using the server master
//      secret (reusing UnwrapDEK / the key logic in crypto.go — no key
//      derivation is duplicated here).
//   3. Encrypt the exact selected span with AES-256-GCM under a fresh random
//      96-bit nonce (Req 16.1).
//   4. Mint a unique token id, INSERT the ciphertext row keyed by that id into
//      CIPHERTEXT, and replace the span in POLYGON.content with the
//      Obfuscation_Token |encrypt|{id}| (Req 16.2).
//
// Steps 3–4 run inside a single transaction so the CIPHERTEXT row and the
// content rewrite commit together or not at all: there is never a window where
// the plaintext has been removed but the ciphertext is missing, nor one where
// the ciphertext exists but the content still holds the plaintext. On any
// failure the transaction rolls back and the stored content is left untouched,
// so NO plaintext copy is ever retained alongside a committed token (Req 16.2).
//
// COORDINATION: the centralized Masker (task 12.3) and per-token reveal (task
// 12.4) extend the same EncryptionService struct. This file keeps that struct
// minimal and additive (a ConnManager plus the master secret) so those tasks
// can hang the read/reveal side off it without redefining anything here.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// gcmNonceLen is the AES-GCM nonce length in bytes (96 bits), the size the GCM
// standard is defined for and what the span cipher uses per Req 16.1.
const gcmNonceLen = 12

// ErrPolygonNotAccessible is returned when the target Polygon is not present in
// the caller's authenticated tenant — whether it does not exist or belongs to
// another tenant. The two cases are indistinguishable by design so nothing
// leaks about records in other tenants (Req 1.6). It mirrors the repo layer's
// not-accessible sentinel while keeping the service package self-contained.
var ErrPolygonNotAccessible = errors.New("polygon not accessible in this tenant")

// ErrInvalidSpan is returned when the requested span offsets do not describe a
// valid, non-empty range within the Polygon's current content. Nothing is
// encrypted and the content is left unchanged.
var ErrInvalidSpan = errors.New("invalid span range")

// EncryptionService performs span encryption/obfuscation and (via later tasks
// 12.3/12.4) centralized masking and per-token reveal. It holds only the
// tenant-routing gateway and the server master secret needed to unwrap the
// per-tenant DEK; all tenant scoping flows through the RequestContext handed to
// each call.
type EncryptionService struct {
	conns        data.ConnManager
	masterSecret string
}

// NewEncryptionService constructs an EncryptionService over the connection
// manager and the server master secret (resolved once at startup, Req 19.3).
// The master secret is what re-derives each tenant's KEK to unwrap its DEK; it
// is never stored per-tenant.
func NewEncryptionService(conns data.ConnManager, masterSecret string) *EncryptionService {
	return &EncryptionService{conns: conns, masterSecret: masterSecret}
}

// EncryptSpanResult reports the outcome of encrypting a span: the minted token
// id (the {id} inside the Obfuscation_Token) and the Polygon's full updated
// content with the span replaced by |encrypt|{id}|. The updated content
// deliberately contains no plaintext copy of the encrypted span (Req 16.2).
type EncryptSpanResult struct {
	TokenID    string
	NewContent string
}

// EncryptSpan encrypts the half-open byte range [start, end) of the Polygon
// identified by polygonRecordID and replaces it with an Obfuscation_Token.
//
// The Polygon is resolved within the caller's own tenant; a Record_ID absent
// from that tenant yields ErrPolygonNotAccessible with no cross-tenant query
// path (Req 1.6). The offsets index the Polygon's current stored content as
// raw bytes; they must satisfy 0 <= start < end <= len(content) and otherwise
// yield ErrInvalidSpan without changing anything.
//
// On success the selected span is encrypted with AES-256-GCM under a fresh
// random 96-bit nonce using the tenant DEK (Req 16.1); a unique token id is
// minted; the ciphertext row (token_id, polygon_id, nonce, ciphertext) is
// inserted into CIPHERTEXT; and POLYGON.content is rewritten with the span
// replaced by |encrypt|{token_id}| — all in one transaction so no plaintext is
// ever retained beside the committed token (Req 16.2).
func (s *EncryptionService) EncryptSpan(ctx context.Context, rc data.RequestContext, polygonRecordID string, start, end int) (EncryptSpanResult, error) {
	pid, err := recordid.Parse(polygonRecordID)
	if err != nil {
		// An unparseable Record_ID can match no row; treat it as not accessible
		// rather than leaking a parse-vs-missing distinction (Req 1.6).
		return EncryptSpanResult{}, ErrPolygonNotAccessible
	}

	tdb, err := s.conns.Tenant(ctx, rc)
	if err != nil {
		if errors.Is(err, data.ErrTenantNotFound) {
			return EncryptSpanResult{}, ErrPolygonNotAccessible
		}
		return EncryptSpanResult{}, fmt.Errorf("resolve tenant: %w", err)
	}
	db := tdb.DB()

	// Resolve the Polygon's internal surrogate id and current content, scoped
	// to this tenant's DB. An absent Record_ID is not accessible (Req 1.6).
	var polygonID int64
	var content string
	err = db.QueryRowContext(ctx,
		`SELECT id, content FROM polygon WHERE record_id = ?`, pid.Canonical(),
	).Scan(&polygonID, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return EncryptSpanResult{}, ErrPolygonNotAccessible
	}
	if err != nil {
		return EncryptSpanResult{}, fmt.Errorf("load polygon content: %w", err)
	}

	// Validate the span against the current content before touching any key
	// material or writing a row. A bad range leaves everything unchanged.
	if start < 0 || end <= start || end > len(content) {
		return EncryptSpanResult{}, ErrInvalidSpan
	}
	plaintext := []byte(content[start:end])

	// Unwrap the tenant DEK using the shared key logic in crypto.go — no key
	// derivation is reimplemented here (design § Key derivation and cipher).
	dek, err := UnwrapDEK(ctx, db, s.masterSecret)
	if err != nil {
		// ErrTenantSecretMissing / ErrUnwrapFailed propagate as-is so callers
		// can distinguish an unprovisioned tenant from a wrong master secret.
		return EncryptSpanResult{}, err
	}

	nonce, ciphertext, err := encryptSpan(dek, plaintext)
	if err != nil {
		return EncryptSpanResult{}, err
	}

	// Mint a unique token id. The Record_ID generator gives a UUID-backed value
	// whose 122 random bits make token-id collisions negligible; CIPHERTEXT's
	// PRIMARY KEY on token_id is the hard uniqueness guarantee (Req 16.1).
	tokenID := recordid.New().Canonical()

	// Build the rewritten content: everything before the span, the token, then
	// everything after. The plaintext bytes are dropped entirely (Req 16.2).
	newContent := content[:start] + obfuscationToken(tokenID) + content[end:]

	// Commit the ciphertext row and the content rewrite together so a token
	// never exists without its ciphertext and plaintext is never left beside a
	// committed token.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return EncryptSpanResult{}, fmt.Errorf("begin encrypt span: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ciphertext (token_id, polygon_id, nonce, ciphertext) VALUES (?, ?, ?, ?)`,
		tokenID, polygonID, nonce, ciphertext,
	); err != nil {
		return EncryptSpanResult{}, fmt.Errorf("insert ciphertext: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE polygon SET content = ? WHERE id = ?`,
		newContent, polygonID,
	); err != nil {
		return EncryptSpanResult{}, fmt.Errorf("rewrite polygon content: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return EncryptSpanResult{}, fmt.Errorf("commit encrypt span: %w", err)
	}

	return EncryptSpanResult{TokenID: tokenID, NewContent: newContent}, nil
}

// obfuscationToken renders the stored placeholder for a token id in the
// Obfuscation_Token form |encrypt|{id}| (Req 16.2, requirements glossary).
func obfuscationToken(tokenID string) string {
	return "|encrypt|" + tokenID + "|"
}

// encryptSpan encrypts plaintext with AES-256-GCM under the 256-bit tenant DEK
// and a fresh random 96-bit nonce, returning the nonce and ciphertext+tag
// separately so they map onto the CIPHERTEXT table's nonce and ciphertext
// columns (Req 16.1). GCM's authentication tag is what later lets reveal detect
// a corrupt or truncated ciphertext (Req 16.8).
func encryptSpan(dek, plaintext []byte) (nonce, ciphertext []byte, err error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("new gcm: %w", err)
	}
	nonce = make([]byte, gcmNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	// Seal with a nil prefix so the returned slice is the bare ciphertext+tag;
	// the nonce is stored in its own column rather than prepended.
	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return nonce, ciphertext, nil
}
