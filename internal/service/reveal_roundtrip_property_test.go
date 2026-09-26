package service

import (
	"context"
	"testing"

	"pgregory.net/rapid"
)

// TestEncryptRevealRoundTripProperty is the property test for the
// encrypt→reveal round-trip guarantee (Requirements 16.1, 16.4).
//
// Feature: influence, Property 17: Encrypt→reveal round-trip
//
// Property 17 (from design.md): For all plaintext spans, encrypting a span and
// then revealing its token as a user holding Reveal_Permission for that span's
// Sphere returns plaintext equal to the original span.
//
// The test drives arbitrary content and an arbitrary valid, non-empty span
// through the real EncryptionService (real file-backed tenant + ConnManager +
// provisioned TENANT_SECRET, via newRevealFixture) and a real PolicyEngine. It
// asserts that RevealToken, invoked with Reveal_Permission granted on the
// token's owning Sphere, returns exactly the original span's plaintext.
//
// The span is drawn to be non-empty because EncryptSpan rejects empty ranges;
// beyond that the content, prefix, suffix, and span bytes are arbitrary so the
// round-trip is exercised across the full input space rather than a fixed
// example. The span offsets are computed from the generated prefix/span lengths
// so the encrypted range is always exactly the drawn span.
//
// Validates: Requirements 16.1, 16.4
func TestEncryptRevealRoundTripProperty(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	// A broad alphabet including spaces, punctuation, digits, and letters so the
	// round-trip is exercised over arbitrary content and arbitrary spans. The
	// span may share bytes with its surroundings — round-trip fidelity does not
	// depend on the span being unique in the content, only on RevealToken
	// returning the exact bytes that were encrypted at the given offsets.
	const alphabet = "abcABC 012 .,-_|"

	rapid.Check(t, func(rt *rapid.T) {
		prefix := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 0, 16, -1).Draw(rt, "prefix")
		// EncryptSpan rejects empty ranges, so the span must be non-empty.
		span := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 1, 24, -1).Draw(rt, "span")
		suffix := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 0, 16, -1).Draw(rt, "suffix")

		content := prefix + span + suffix
		polyRID := seedPolygon(t, f.db, content)

		start := len(prefix)
		end := start + len(span)

		res, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), start, end)
		if err != nil {
			rt.Fatalf("EncryptSpan(content=%q, [%d,%d)) error = %v", content, start, end, err)
		}

		// Grant Reveal_Permission on the token's owning Sphere, then reveal.
		sphere := sphereIDOf(t, f, polyRID)
		plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, revealAuthz(sphere, true), res.TokenID)
		if err != nil {
			rt.Fatalf("RevealToken(token=%q) error = %v", res.TokenID, err)
		}

		// The revealed plaintext must equal the original span byte-for-byte.
		if plain != span {
			rt.Fatalf("revealed plaintext = %q, want original span %q (content=%q, [%d,%d))",
				plain, span, content, start, end)
		}
	})
}
