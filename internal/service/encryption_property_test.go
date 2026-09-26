package service

import (
	"context"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestEncryptionObfuscationRemovesPlaintextProperty is the property test for the
// encryption obfuscation guarantee (Requirements 16.1, 16.2).
//
// Feature: influence, Property 16: Encryption obfuscation removes plaintext
//
// Property 16 (from design.md): For all selected content spans, after
// encryption the stored content contains an Obfuscation_Token of the form
// |encrypt|{id}| in place of the span, contains no copy of the span's
// plaintext, and stores the corresponding ciphertext keyed by that id.
//
// The test drives arbitrary content and an arbitrary valid span through the real
// EncryptionService (real file-backed tenant + ConnManager + provisioned
// TENANT_SECRET, via newEncryptFixture) and asserts, after EncryptSpan:
//
//   - the stored POLYGON.content equals prefix + |encrypt|{id}| + suffix, so the
//     token stands exactly where the span was;
//   - the stored content contains no copy of the span's plaintext; and
//   - a CIPHERTEXT row keyed by the returned token id exists, is non-empty, and
//     does not contain the span's plaintext bytes.
//
// To keep "contains no copy of the span's plaintext" a faithful, decidable
// property, the span is drawn from an alphabet disjoint both from the
// surrounding prefix/suffix and from the characters that appear in the
// |encrypt|{id}| token (its UUID id uses only hex digits and '-'). Otherwise a
// span whose bytes happened to also occur elsewhere in the content — including
// inside the token id itself — would legitimately still appear after
// encryption, which the requirement does not forbid (it only removes the
// encrypted span). The disjoint alphabet makes the span the sole occurrence, so
// its total absence is exactly the property under test.
//
// Validates: Requirements 16.1, 16.2
func TestEncryptionObfuscationRemovesPlaintextProperty(t *testing.T) {
	f := newEncryptFixture(t)
	ctx := context.Background()

	// Surrounding text uses only these bytes; the secret span uses only the
	// distinct bytes below. The secret alphabet is disjoint from both the
	// surroundings AND from the characters a UUID token id can contain (hex
	// digits 0-9a-f and '-'), so an occurrence of the span in the stored content
	// can only be a retained plaintext copy — never a coincidental match inside
	// the surrounding text or inside the |encrypt|{id}| token itself.
	const surroundAlphabet = "abc def"
	const secretAlphabet = "GHIJKLMNOPQRSTUVWXYZ"

	rapid.Check(t, func(rt *rapid.T) {
		prefix := rapid.StringOfN(rapid.SampledFrom([]rune(surroundAlphabet)), 0, 12, -1).Draw(rt, "prefix")
		suffix := rapid.StringOfN(rapid.SampledFrom([]rune(surroundAlphabet)), 0, 12, -1).Draw(rt, "suffix")
		// The span must be non-empty (EncryptSpan rejects empty ranges). A
		// minimum length of 4 keeps the "ciphertext does not contain the
		// plaintext" check meaningful: AES-GCM ciphertext bytes are effectively
		// random, so any single byte value will eventually appear by chance,
		// but a contiguous run of 4+ specific bytes matching the plaintext by
		// chance is negligibly unlikely. Verbatim retention of the plaintext —
		// the failure this guards against — is a contiguous multi-byte match and
		// is still reliably caught.
		span := rapid.StringOfN(rapid.SampledFrom([]rune(secretAlphabet)), 4, 16, -1).Draw(rt, "span")

		content := prefix + span + suffix
		polyRID := seedPolygon(t, f.db, content)

		start := len(prefix)
		end := start + len(span)

		res, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), start, end)
		if err != nil {
			rt.Fatalf("EncryptSpan(content=%q, [%d,%d)) error = %v", content, start, end, err)
		}

		token := "|encrypt|" + res.TokenID + "|"
		wantContent := prefix + token + suffix

		// The stored content must place the token exactly where the span was.
		stored := readContent(t, f.db, polyRID)
		if stored != wantContent {
			rt.Fatalf("stored content = %q, want %q", stored, wantContent)
		}
		// The returned content must agree with what was stored.
		if res.NewContent != wantContent {
			rt.Fatalf("returned content = %q, want %q", res.NewContent, wantContent)
		}
		// The Obfuscation_Token must be present...
		if !strings.Contains(stored, token) {
			rt.Fatalf("stored content %q missing obfuscation token %q", stored, token)
		}
		// ...and no copy of the span's plaintext may remain (the span alphabet is
		// disjoint from the surroundings, so any occurrence would be the span).
		if strings.Contains(stored, span) {
			rt.Fatalf("stored content %q still contains plaintext span %q", stored, span)
		}

		// A CIPHERTEXT row keyed by the token id must exist, be non-empty, and
		// not contain the plaintext span bytes.
		var nonce, ct []byte
		if err := f.db.QueryRow(
			`SELECT nonce, ciphertext FROM ciphertext WHERE token_id = ?`, res.TokenID,
		).Scan(&nonce, &ct); err != nil {
			rt.Fatalf("ciphertext row for token %q: %v", res.TokenID, err)
		}
		if len(nonce) != gcmNonceLen {
			rt.Fatalf("nonce length = %d, want %d", len(nonce), gcmNonceLen)
		}
		if len(ct) == 0 {
			rt.Fatalf("ciphertext for token %q is empty", res.TokenID)
		}
		if strings.Contains(string(ct), span) {
			rt.Fatalf("ciphertext row for token %q contains plaintext span %q", res.TokenID, span)
		}
	})
}
