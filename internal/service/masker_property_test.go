package service

import (
	"context"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestMaskingNeverLeaksPlaintextOrCiphertextProperty is the highest-priority
// security property for the platform (Requirements 16.3, 16.7, 22.2, 23.2).
//
// Feature: influence, Property 18: Masking never leaks plaintext or ciphertext
//
// Property 18 (from design.md): For all content containing Obfuscation_Tokens
// rendered, exported to PDF, or indexed for search in a context lacking
// Reveal_Permission for the relevant Sphere, the produced output contains only
// masked placeholders and includes neither the plaintext nor the ciphertext of
// any token.
//
// The design ("Masking at every egress") establishes that a single Masker is
// the only thing that turns stored tokens into observable output, and that all
// three egress paths route through it in a no-reveal context:
//
//   - Render: Mask(content, reveal=false, ...) for a viewer without
//     Reveal_Permission (Req 16.3).
//   - PDF export: the PDF is produced from the app's own rendered HTML for the
//     requesting user, so it inherits the same masked render (Req 22.2).
//   - Search index: the FTS row is built from masked content (Req 16.7, 23.2).
//
// This test therefore models the three egress paths as three invocations of the
// same Mask seam with a NO-REVEAL context — reveal=false with a nil revealer,
// and (redundantly, to exercise both no-reveal shapes) reveal=true with a nil
// revealer. For arbitrary content carrying one or more real Obfuscation_Tokens
// minted by the production EncryptionService, every egress output must contain
// NEITHER the plaintext of ANY encrypted span NOR the ciphertext bytes of ANY
// token, and must leave no raw token behind.
//
// To keep "contains no plaintext" a faithful, decidable property, each secret
// span is drawn from an alphabet disjoint from the surrounding text and from
// every character an |encrypt|{id}| token or the masked placeholder can contain
// (UUID ids use only hex digits 0-9a-f and '-'; the placeholder uses only its
// own fixed runes). Any occurrence of a span's bytes in an egress output could
// then only be a leaked plaintext copy, never a coincidental match. The actual
// ciphertext bytes are read back from the CIPHERTEXT rows and checked directly,
// so ciphertext-absence is asserted against the real stored bytes rather than a
// stand-in.
//
// Validates: Requirements 16.3, 16.7, 22.2, 23.2
func TestMaskingNeverLeaksPlaintextOrCiphertextProperty(t *testing.T) {
	f := newEncryptFixture(t)
	ctx := context.Background()

	// Surrounding text and the secret spans use disjoint alphabets. The secret
	// alphabet is also disjoint from the characters a UUID token id (hex + '-')
	// and the masked placeholder can contain, so any occurrence of a span in an
	// egress output can only be a retained plaintext copy.
	const surroundAlphabet = "abc def"
	const secretAlphabet = "GHIJKLMNOPQRSTUVWXYZ"

	rapid.Check(t, func(rt *rapid.T) {
		// One or more encrypted spans. Each span is separated by surrounding
		// (non-secret) text so the resulting content is a realistic mix of
		// masked and plain regions.
		nSpans := rapid.IntRange(1, 4).Draw(rt, "nSpans")

		var contentBuilder strings.Builder
		spans := make([]string, 0, nSpans)
		spanRanges := make([][2]int, 0, nSpans)

		for i := 0; i < nSpans; i++ {
			prefix := rapid.StringOfN(rapid.SampledFrom([]rune(surroundAlphabet)), 0, 8, -1).Draw(rt, "prefix")
			// Spans are 4+ bytes so a chance contiguous match against random
			// ciphertext bytes is negligible; verbatim retention (the failure
			// under test) is a contiguous multi-byte match and is still caught.
			span := rapid.StringOfN(rapid.SampledFrom([]rune(secretAlphabet)), 4, 12, -1).Draw(rt, "span")

			contentBuilder.WriteString(prefix)
			start := contentBuilder.Len()
			contentBuilder.WriteString(span)
			end := contentBuilder.Len()

			spans = append(spans, span)
			spanRanges = append(spanRanges, [2]int{start, end})
		}
		// A trailing chunk of surrounding text after the last span.
		contentBuilder.WriteString(rapid.StringOfN(rapid.SampledFrom([]rune(surroundAlphabet)), 0, 8, -1).Draw(rt, "trailing"))

		content := contentBuilder.String()
		polyRID := seedPolygon(t, f.db, content)

		// Encrypt each span in place. Because encrypting a span rewrites the
		// content (replacing bytes with a token of a different length), we walk
		// the spans from LAST to FIRST so earlier byte offsets stay valid as we
		// go. Each EncryptSpan stores a CIPHERTEXT row and swaps the span for an
		// |encrypt|{id}| token.
		tokenIDs := make([]string, len(spans))
		for i := len(spanRanges) - 1; i >= 0; i-- {
			start, end := spanRanges[i][0], spanRanges[i][1]
			res, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), start, end)
			if err != nil {
				rt.Fatalf("EncryptSpan(span=%q, [%d,%d)) error = %v", spans[i], start, end, err)
			}
			tokenIDs[i] = res.TokenID
		}

		// The stored content now holds only tokens where the spans were; this is
		// exactly what every egress reads from.
		stored := readContent(t, f.db, polyRID)
		if strings.Contains(stored, "|encrypt|") == false {
			rt.Fatalf("stored content %q has no obfuscation tokens after encrypting %d spans", stored, nSpans)
		}

		// Read back the ACTUAL ciphertext bytes for every token so we assert
		// ciphertext-absence against the real stored bytes, not a stand-in.
		ciphertexts := make([]string, 0, len(tokenIDs))
		for _, tid := range tokenIDs {
			var ct []byte
			if err := f.db.QueryRow(
				`SELECT ciphertext FROM ciphertext WHERE token_id = ?`, tid,
			).Scan(&ct); err != nil {
				rt.Fatalf("read ciphertext for token %q: %v", tid, err)
			}
			if len(ct) == 0 {
				rt.Fatalf("ciphertext for token %q is empty", tid)
			}
			ciphertexts = append(ciphertexts, string(ct))
		}

		// Model the three egress paths, each routing the stored content through
		// the single Mask seam in a NO-REVEAL context. Render, PDF export, and
		// search-index population all read masked content per the design; here
		// they differ only in which no-reveal shape they use, and both shapes
		// must be safe.
		egresses := map[string]string{
			// Render for a viewer without Reveal_Permission (Req 16.3).
			"render": Mask(stored, false, nil),
			// PDF export inherits the masked render (Req 22.2). A revealer that
			// WOULD leak is supplied to prove reveal=false never consults it.
			"pdf": Mask(stored, false, func(tokenID string) (string, bool) {
				rt.Fatalf("revealer must not be called for a no-reveal PDF egress")
				return spans[0], true
			}),
			// Search-index population from masked content (Req 16.7, 23.2). A
			// nil revealer with reveal=true is also a no-reveal context.
			"search-index": Mask(stored, true, nil),
		}

		for name, out := range egresses {
			// No raw token may survive any egress.
			if strings.Contains(out, "|encrypt|") {
				rt.Fatalf("%s egress left a raw token: %q", name, out)
			}
			// No span's plaintext may appear (disjoint alphabet makes any
			// occurrence a genuine leak).
			for i, span := range spans {
				if strings.Contains(out, span) {
					rt.Fatalf("%s egress leaked plaintext of span %d (%q): %q", name, i, span, out)
				}
			}
			// No token's actual ciphertext bytes may appear.
			for i, ct := range ciphertexts {
				if strings.Contains(out, ct) {
					rt.Fatalf("%s egress leaked ciphertext of token %d (%q): %q", name, i, tokenIDs[i], out)
				}
			}
			// Every token must have become the masked placeholder; there must be
			// exactly one placeholder per encrypted span.
			if n := strings.Count(out, MaskedPlaceholder); n != nSpans {
				rt.Fatalf("%s egress: expected %d masked placeholders, got %d in %q", name, nSpans, n, out)
			}
		}
	})
}
