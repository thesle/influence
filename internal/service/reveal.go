// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package service

// Per-token reveal with per-Sphere permission (design § Reveal flow,
// Requirements 16.4, 16.5, 16.6, 16.8).
//
// This is the read side of the encryption feature and the concrete
// implementation the Masker's Revealer callback (task 12.3) defers to. Stored
// POLYGON.content never holds plaintext; a sensitive span is an
// Obfuscation_Token |encrypt|{id}| that maps to a CIPHERTEXT row. Rendering
// masks every token by default; a user with Reveal_Permission may request a
// SINGLE token, and only that token is decrypted and returned — every other
// token stays masked until individually requested (Req 16.4).
//
// The flow mirrors the design's Reveal sequence exactly:
//
//   1. Resolve the target token's CIPHERTEXT row within the caller's own tenant
//      (the tenant handle comes from RequestContext, never client input), and
//      with it the token's owning Polygon and that Polygon's Sphere
//      (token_id -> polygon_id -> polygon.sphere_id).
//   2. RE-CHECK reveal permission for THAT Sphere via PolicyEngine.CanReveal.
//      Reveal is strictly per-Sphere (Req 16.6): a user permitted in one Sphere
//      may not reveal a token whose owning Polygon lives in another. A caller
//      lacking permission is denied with ErrRevealDenied; the content stays
//      masked and no plaintext is produced (Req 16.5).
//   3. Unwrap the tenant DEK (shared key logic in crypto.go) and AES-256-GCM
//      open the stored nonce+ciphertext. A missing row or a GCM authentication
//      failure yields ErrRevealFailed with NO partial plaintext (Req 16.8).
//   4. On success return just that one token's plaintext (Req 16.4).
//
// The permission check runs BEFORE any decryption so an unauthorized caller
// never causes key material to touch the token's bytes, and the two failure
// sentinels are deliberately distinct so a caller (and the HTTP layer's
// REVEAL_DENIED vs. reveal-failed envelope) can tell "not permitted" from
// "ciphertext missing/undecryptable" while neither ever carries plaintext.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"errors"
	"fmt"

	"github.com/influence/influence/internal/data"
)

// ErrRevealDenied is returned when the caller lacks Reveal_Permission for the
// Sphere that owns the requested token. The content stays masked and no
// plaintext is produced (Req 16.5, 16.6). It maps to the API's REVEAL_DENIED
// error envelope.
var ErrRevealDenied = errors.New("reveal not permitted for this sphere")

// ErrRevealFailed is returned when the requested token's ciphertext is missing
// or cannot be decrypted (GCM authentication failure). The content stays masked
// and NO partial plaintext is exposed (Req 16.8). It is deliberately distinct
// from ErrRevealDenied but, like it, never carries any plaintext.
var ErrRevealFailed = errors.New("reveal failed")

// RevealToken reveals a single Obfuscation_Token's Sensitive_Data for a caller
// who holds Reveal_Permission for the token's owning Sphere.
//
// It resolves the CIPHERTEXT row for tokenID within the caller's own tenant,
// determines the owning Sphere via the token's Polygon (token_id -> polygon_id
// -> polygon.sphere_id), and RE-CHECKS reveal permission for THAT Sphere with
// policy.CanReveal (Req 16.6). A caller lacking permission is denied with
// ErrRevealDenied and no plaintext is produced (Req 16.5). If the token's
// ciphertext is missing or fails AES-256-GCM authentication, it returns
// ErrRevealFailed with no partial plaintext (Req 16.8). Otherwise it decrypts
// under the tenant DEK and returns that single token's plaintext (Req 16.4).
//
// Only the requested token is revealed; RevealToken has no effect on any other
// token, which remains masked until individually requested.
func (s *EncryptionService) RevealToken(ctx context.Context, rc data.RequestContext, policy PolicyEngine, authz AuthzData, tokenID string) (string, error) {
	tdb, err := s.conns.Tenant(ctx, rc)
	if err != nil {
		if errors.Is(err, data.ErrTenantNotFound) {
			// No reachable tenant means no reachable token: treat as a failed
			// reveal rather than leaking a tenant-resolution distinction.
			return "", ErrRevealFailed
		}
		return "", fmt.Errorf("resolve tenant: %w", err)
	}
	db := tdb.DB()

	// Resolve the token's ciphertext row and its owning Sphere in one query.
	// The join to polygon yields the Sphere the reveal permission is keyed on
	// (Req 16.6). A token absent from this tenant's CIPHERTEXT (unknown id or a
	// token that belongs to another tenant's file, which this DB cannot see)
	// yields no row and is reported as a failed reveal without partial output
	// (Req 16.8).
	var sphereID data.SphereID
	var nonce, ciphertext []byte
	err = db.QueryRowContext(ctx,
		`SELECT p.sphere_id, c.nonce, c.ciphertext
		   FROM ciphertext c
		   JOIN polygon p ON p.id = c.polygon_id
		  WHERE c.token_id = ?`,
		tokenID,
	).Scan(&sphereID, &nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrRevealFailed
	}
	if err != nil {
		return "", fmt.Errorf("load ciphertext: %w", err)
	}

	// Re-check reveal permission for the token's OWNING Sphere before any key
	// material touches the ciphertext. Denial keeps the content masked and
	// produces no plaintext (Req 16.5, 16.6).
	if !policy.CanReveal(rc, authz, sphereID) {
		return "", ErrRevealDenied
	}

	// Unwrap the tenant DEK using the shared key logic in crypto.go — no key
	// derivation is reimplemented here. An unprovisioned or unrecoverable key
	// means the token cannot be decrypted, which is a reveal failure with no
	// partial plaintext (Req 16.8).
	dek, err := UnwrapDEK(ctx, db, s.masterSecret)
	if err != nil {
		return "", ErrRevealFailed
	}

	plaintext, err := decryptSpan(dek, nonce, ciphertext)
	if err != nil {
		// GCM authentication failure (corrupt/truncated ciphertext or wrong
		// key) surfaces as a failed reveal; Open returns no bytes on failure so
		// no partial plaintext can escape (Req 16.8).
		return "", ErrRevealFailed
	}
	return string(plaintext), nil
}

// decryptSpan reverses encryptSpan (encryption.go): it AES-256-GCM opens the
// stored nonce+ciphertext under the tenant DEK. GCM's authentication tag makes
// a corrupt, truncated, or wrong-key ciphertext fail here rather than yielding
// garbage, and Open returns nil on failure so no partial plaintext is ever
// produced (Req 16.8).
func decryptSpan(dek, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		// A nonce of the wrong size can never authenticate; treat it as a
		// decryption failure rather than passing it to Open.
		return nil, fmt.Errorf("bad nonce length %d", len(nonce))
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("gcm open: %w", err)
	}
	return plaintext, nil
}
