package service

import (
	"context"
	"testing"

	"github.com/influence/influence/internal/recordid"
)

// This file adds net-new example assertions for task 12.10, focused on the two
// distinct negative reveal responses:
//
//   - REVEAL DENIED (Req 16.5): a caller without Reveal_Permission is denied
//     with ErrRevealDenied and no plaintext.
//   - REVEAL FAILED (Req 16.8): a missing OR undecryptable ciphertext returns
//     ErrRevealFailed with NO partial plaintext, across several corruption forms.
//
// The single-form cases (one corruption, one missing id, one no-permission) are
// already covered by reveal_test.go; the tests here strengthen that coverage by
// (a) sweeping multiple corrupt/truncated ciphertext shapes to assert none ever
// leaks a non-empty plaintext, and (b) asserting the two sentinels are distinct
// so the API layer can map REVEAL_DENIED vs. reveal-failed without either
// response carrying plaintext.

// TestRevealDeniedAndFailedSentinelsAreDistinct verifies ErrRevealDenied and
// ErrRevealFailed are different errors. The HTTP layer relies on this to emit a
// REVEAL_DENIED envelope for permission failures and a separate reveal-failed
// envelope for missing/undecryptable ciphertext (Req 16.5 vs. 16.8), and neither
// path may carry plaintext.
func TestRevealDeniedAndFailedSentinelsAreDistinct(t *testing.T) {
	if ErrRevealDenied == ErrRevealFailed {
		t.Fatal("ErrRevealDenied and ErrRevealFailed must be distinct sentinels")
	}
	if ErrRevealDenied.Error() == ErrRevealFailed.Error() {
		t.Errorf("denied/failed error messages must differ: %q == %q",
			ErrRevealDenied.Error(), ErrRevealFailed.Error())
	}
}

// TestRevealDeniedResponseCarriesNoPlaintext re-confirms, as an explicit example
// for Req 16.5, that a no-permission reveal yields exactly ErrRevealDenied with
// an empty string and never the underlying plaintext — even though the token's
// ciphertext exists and is perfectly decryptable for a permitted caller.
func TestRevealDeniedResponseCarriesNoPlaintext(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	const secret = "TOP-SECRET-VALUE"
	tokenID, polyRID := encryptOneSpan(t, f, "before "+secret+" after", secret)
	sphere := sphereIDOf(t, f, polyRID)

	// read granted, reveal NOT granted on the owning Sphere.
	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, revealAuthz(sphere, false), tokenID)
	if err != ErrRevealDenied {
		t.Fatalf("error = %v, want ErrRevealDenied", err)
	}
	if plain != "" {
		t.Errorf("denied reveal returned plaintext %q, want empty", plain)
	}
	if plain == secret {
		t.Errorf("denied reveal leaked the secret plaintext")
	}
}

// TestRevealFailedNeverLeaksPlaintextAcrossCorruptForms sweeps several shapes of
// broken stored ciphertext and asserts each one returns ErrRevealFailed with an
// empty string — no partial plaintext escapes regardless of how the ciphertext
// is malformed (Req 16.8). A permissive admin authz isolates the failure to
// decryption rather than permission.
func TestRevealFailedNeverLeaksPlaintextAcrossCorruptForms(t *testing.T) {
	ctx := context.Background()

	const secret = "REVEAL-FAILED-SECRET"
	corruptions := map[string][]byte{
		"empty ciphertext":      {},
		"single byte":           {0x00},
		"short garbage":         []byte("nope"),
		"long garbage":          []byte("this-is-not-a-valid-gcm-ciphertext-at-all-really"),
		"all zero bytes":        make([]byte, 32),
		"truncated-length like": []byte("aaaaaaaaaaaaaaaa"), // 16 bytes, plausible-looking but unauthenticated
	}

	for name, corrupt := range corruptions {
		corrupt := corrupt
		t.Run(name, func(t *testing.T) {
			// Fresh fixture per form so each corruption is independent.
			f := newRevealFixture(t)
			tokenID, _ := encryptOneSpan(t, f, "x "+secret+" y", secret)

			if _, err := f.db.Exec(
				`UPDATE ciphertext SET ciphertext = ? WHERE token_id = ?`,
				corrupt, tokenID,
			); err != nil {
				t.Fatalf("corrupt ciphertext: %v", err)
			}

			plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, AuthzData{Admin: true}, tokenID)
			if err != ErrRevealFailed {
				t.Fatalf("error = %v, want ErrRevealFailed", err)
			}
			if plain != "" {
				t.Errorf("failed reveal returned %q, want empty (no partial plaintext)", plain)
			}
		})
	}
}

// TestRevealFailedForCorruptNonceCarriesNoPlaintext corrupts the stored nonce
// (rather than the ciphertext body) to a wrong length so GCM can never
// authenticate, and confirms the reveal still fails cleanly with no partial
// plaintext (Req 16.8).
func TestRevealFailedForCorruptNonceCarriesNoPlaintext(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	tokenID, _ := encryptOneSpan(t, f, "n SECRET m", "SECRET")

	if _, err := f.db.Exec(
		`UPDATE ciphertext SET nonce = ? WHERE token_id = ?`,
		[]byte{0x01, 0x02, 0x03}, tokenID, // wrong nonce length
	); err != nil {
		t.Fatalf("corrupt nonce: %v", err)
	}

	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, AuthzData{Admin: true}, tokenID)
	if err != ErrRevealFailed {
		t.Fatalf("error = %v, want ErrRevealFailed", err)
	}
	if plain != "" {
		t.Errorf("corrupt-nonce reveal returned %q, want empty", plain)
	}
}

// TestRevealFailedForMissingTokenCarriesNoPlaintext re-asserts, as an explicit
// example for Req 16.8, that an entirely unknown token id (no ciphertext row)
// fails with ErrRevealFailed and empty plaintext under a permissive authz, so
// the failure is attributable to the absent ciphertext and not permission.
func TestRevealFailedForMissingTokenCarriesNoPlaintext(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, AuthzData{Admin: true}, recordid.New().Canonical())
	if err != ErrRevealFailed {
		t.Fatalf("error = %v, want ErrRevealFailed", err)
	}
	if plain != "" {
		t.Errorf("missing-token reveal returned %q, want empty", plain)
	}
}
