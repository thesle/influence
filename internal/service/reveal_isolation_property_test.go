package service

import (
	"context"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestRevealTokenPerTokenIsolationProperty is the property test for per-token
// reveal isolation (Requirement 16.4).
//
// Feature: influence, Property 19: Per-token reveal isolation
//
// Property 19 (from design.md): For all content containing two or more
// Obfuscation_Tokens, revealing a single token for an authorized user exposes
// only that token's plaintext while every other token remains masked.
//
// The test drives arbitrary content with N (>= 2) encrypted spans through the
// real EncryptionService (real file-backed tenant + ConnManager + provisioned
// TENANT_SECRET, via the shared reveal fixture). It encrypts each span in turn,
// re-reading the stored content between encryptions exactly as the production
// call sequence would, so the stored POLYGON.content ends up holding N distinct
// |encrypt|{id}| tokens. It then picks one target token and asserts the two
// halves of the isolation guarantee:
//
//   - RevealToken for the target returns exactly the target span's plaintext and
//     nothing else; and
//   - rendering the stored content through the single masking egress (Mask) with
//     a Revealer that permits ONLY the target token yields the target plaintext
//     while every OTHER token renders as MaskedPlaceholder — no other span's
//     plaintext, no |encrypt| token marker, and no ciphertext appears.
//
// Each secret span is drawn from a per-index alphabet that is disjoint from
// every other span's alphabet, from the surrounding text, and from the
// characters a UUID token id can contain (hex digits and '-'). That keeps
// "only the target's plaintext appears" a decidable property: any occurrence of
// a non-target span in the rendered output could only be a leaked reveal, never
// a coincidental match in the surroundings or inside a token id.
//
// Validates: Requirements 16.4
func TestRevealTokenPerTokenIsolationProperty(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	// Surrounding text uses only these bytes. Each secret span uses a distinct
	// single-letter alphabet from secretAlphabets[i]; all are disjoint from the
	// surroundings and from UUID token-id characters (0-9a-f and '-'), so every
	// span is the sole possible source of its own bytes in any rendered output.
	const surroundAlphabet = "xyz "
	secretAlphabets := []string{"G", "H", "I", "J"}

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(2, len(secretAlphabets)).Draw(rt, "numTokens")

		// Build content as: sep span0 sep span1 sep ... spanN-1 sep
		// where each span is a run of its own distinct letter (length 3..8) and
		// each separator is drawn from the surrounding alphabet. Distinct span
		// letters guarantee the spans are pairwise distinct and each occurs
		// exactly once in the original content.
		var b strings.Builder
		spans := make([]string, n)
		for i := 0; i < n; i++ {
			sep := rapid.StringOfN(rapid.SampledFrom([]rune(surroundAlphabet)), 1, 4, -1).Draw(rt, "sep")
			b.WriteString(sep)
			runLen := rapid.IntRange(3, 8).Draw(rt, "spanLen")
			span := strings.Repeat(secretAlphabets[i], runLen)
			spans[i] = span
			b.WriteString(span)
		}
		trailer := rapid.StringOfN(rapid.SampledFrom([]rune(surroundAlphabet)), 1, 4, -1).Draw(rt, "trailer")
		b.WriteString(trailer)
		content := b.String()

		polyRID := seedPolygon(t, f.db, content)

		// Encrypt each span in turn, re-reading the stored content between
		// encryptions so each token id lands exactly where its span was. After
		// the first encryption the span text is gone, but each span's letter is
		// unique so locating the (single) remaining occurrence stays correct.
		tokenIDs := make([]string, n)
		for i := 0; i < n; i++ {
			stored := readContent(t, f.db, polyRID)
			start := strings.Index(stored, spans[i])
			if start < 0 {
				rt.Fatalf("span %d %q not found in stored content %q", i, spans[i], stored)
			}
			res, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), start, start+len(spans[i]))
			if err != nil {
				rt.Fatalf("EncryptSpan(span %d) error = %v", i, err)
			}
			tokenIDs[i] = res.TokenID
		}

		sphere := sphereIDOf(t, f, polyRID)
		authz := revealAuthz(sphere, true)

		// Reveal exactly ONE token. Iterate all indices as the target so every
		// token gets exercised as both "the revealed one" and "a masked other".
		target := rapid.IntRange(0, n-1).Draw(rt, "target")

		// Direct-call half: RevealToken returns only the target's plaintext.
		got, err := f.svc.RevealToken(ctx, f.rc, f.policy, authz, tokenIDs[target])
		if err != nil {
			rt.Fatalf("RevealToken(target %d) error = %v", target, err)
		}
		if got != spans[target] {
			rt.Fatalf("RevealToken(target %d) = %q, want %q", target, got, spans[target])
		}

		// Masking-egress half: a Revealer permitting ONLY the target token, so
		// the rendered output reveals the target and masks every other token.
		revealer := func(tokenID string) (string, bool) {
			if tokenID != tokenIDs[target] {
				// Not the requested token: keep it masked. This mirrors a
				// single-token reveal request in the render path.
				return "", false
			}
			plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, authz, tokenID)
			if err != nil {
				return "", false
			}
			return plain, true
		}

		stored := readContent(t, f.db, polyRID)
		rendered := Mask(stored, true, revealer)

		// The target's plaintext must appear exactly once (it was a single run
		// in the original content and its alphabet is unique to it).
		if c := strings.Count(rendered, spans[target]); c != 1 {
			rt.Fatalf("rendered output contains target plaintext %q %d time(s), want 1: %q",
				spans[target], c, rendered)
		}

		// Every OTHER token must remain masked: its plaintext must be absent,
		// and a MaskedPlaceholder must stand where it was.
		for i := 0; i < n; i++ {
			if i == target {
				continue
			}
			if strings.Contains(rendered, spans[i]) {
				rt.Fatalf("rendered output leaked non-target plaintext %q (token %d): %q",
					spans[i], i, rendered)
			}
		}

		// The number of masked placeholders must equal the number of
		// non-revealed tokens (n-1): no token was dropped and none over-masked.
		if got, want := strings.Count(rendered, MaskedPlaceholder), n-1; got != want {
			rt.Fatalf("rendered output has %d masked placeholders, want %d: %q",
				got, want, rendered)
		}

		// Neither the raw token marker nor any ciphertext bytes may survive to
		// the egress. No |encrypt| marker should remain, and none of the stored
		// ciphertext for the masked tokens should appear in the output.
		if strings.Contains(rendered, "|encrypt|") {
			rt.Fatalf("rendered output still contains a raw obfuscation token marker: %q", rendered)
		}
		for i := 0; i < n; i++ {
			if i == target {
				continue
			}
			var ct []byte
			if err := f.db.QueryRow(
				`SELECT ciphertext FROM ciphertext WHERE token_id = ?`, tokenIDs[i],
			).Scan(&ct); err != nil {
				rt.Fatalf("read ciphertext for token %d: %v", i, err)
			}
			if len(ct) > 0 && strings.Contains(rendered, string(ct)) {
				rt.Fatalf("rendered output leaked ciphertext for masked token %d: %q", i, rendered)
			}
		}
	})
}
