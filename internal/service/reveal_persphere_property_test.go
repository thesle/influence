package service

import (
	"context"
	"testing"

	"github.com/influence/influence/internal/data"
	"pgregory.net/rapid"
)

// TestRevealPermissionIsPerSphereProperty is the property test for the
// per-Sphere reveal permission guarantee (Requirements 16.5, 16.6).
//
// Feature: influence, Property 20: Reveal permission is per-Sphere
//
// Property 20 (from design.md): For all users granted Reveal_Permission on one
// Sphere but not another, a reveal request succeeds for tokens whose owning
// Polygon is in the granted Sphere and is denied (content staying masked) for
// tokens in any other Sphere.
//
// The test encrypts an arbitrary span through the real EncryptionService (real
// file-backed tenant + ConnManager + provisioned TENANT_SECRET via
// newRevealFixture) so RevealToken runs the exact production tenant-routing,
// key-unwrap, and per-Sphere permission path. It then drives RevealToken with a
// grant placed either on the token's OWNING Sphere or on some DIFFERENT Sphere,
// with Reveal_Permission either enabled or not, and asserts the single
// invariant that captures Property 20:
//
//   - RevealToken returns the original plaintext IF AND ONLY IF the caller's
//     grant is on the token's OWNING Sphere with Reveal_Permission enabled.
//   - Every other case — permission granted on a DIFFERENT Sphere (per-Sphere
//     isolation, Req 16.6), or the grant on the owning Sphere but without
//     Reveal_Permission (Req 16.5) — is denied with ErrRevealDenied and yields
//     no plaintext (empty string), so the content stays masked.
//
// Validates: Requirements 16.5, 16.6
func TestRevealPermissionIsPerSphereProperty(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	// The secret span is drawn from an alphabet disjoint from the surrounding
	// text so the recovered plaintext equality check is unambiguous. A length
	// of 4+ keeps EncryptSpan happy (it rejects empty ranges) and the equality
	// comparison meaningful.
	const surroundAlphabet = "abc def"
	const secretAlphabet = "GHIJKLMNOPQRSTUVWXYZ"

	rapid.Check(t, func(rt *rapid.T) {
		prefix := rapid.StringOfN(rapid.SampledFrom([]rune(surroundAlphabet)), 0, 12, -1).Draw(rt, "prefix")
		suffix := rapid.StringOfN(rapid.SampledFrom([]rune(surroundAlphabet)), 0, 12, -1).Draw(rt, "suffix")
		span := rapid.StringOfN(rapid.SampledFrom([]rune(secretAlphabet)), 4, 16, -1).Draw(rt, "span")

		content := prefix + span + suffix
		tokenID, polyRID := encryptOneSpan(t, f, content, span)
		owningSphere := sphereIDOf(t, f, polyRID)

		// Choose whether the grant is placed on the token's owning Sphere or on
		// a different one, and whether Reveal_Permission is enabled. A positive,
		// non-zero offset guarantees the "different" Sphere id can never collide
		// with the owning Sphere.
		grantOnOwning := rapid.Bool().Draw(rt, "grantOnOwning")
		reveal := rapid.Bool().Draw(rt, "reveal")
		offset := rapid.Int64Range(1, 1_000_000).Draw(rt, "sphereOffset")

		grantSphere := owningSphere
		if !grantOnOwning {
			grantSphere = owningSphere + data.SphereID(offset)
		}
		authz := revealAuthz(grantSphere, reveal)

		plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, authz, tokenID)

		// Reveal is permitted IFF the grant is on the OWNING Sphere AND
		// Reveal_Permission is enabled.
		wantPermitted := grantOnOwning && reveal

		if wantPermitted {
			if err != nil {
				rt.Fatalf("RevealToken(grantOnOwning=%v, reveal=%v) error = %v, want success",
					grantOnOwning, reveal, err)
			}
			if plain != span {
				rt.Fatalf("revealed plaintext = %q, want %q", plain, span)
			}
			return
		}

		// Not permitted: denied with ErrRevealDenied and no plaintext leaks, so
		// the content stays masked (Req 16.5, 16.6).
		if err != ErrRevealDenied {
			rt.Fatalf("RevealToken(grantOnOwning=%v, reveal=%v) error = %v, want ErrRevealDenied",
				grantOnOwning, reveal, err)
		}
		if plain != "" {
			rt.Fatalf("denied reveal returned plaintext %q, want empty", plain)
		}
	})
}
