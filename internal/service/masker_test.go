package service

import (
	"strings"
	"testing"
)

// The ciphertext for a token never appears in stored content (only the token
// does), but these tests thread a distinctive "ciphertext" sentinel through the
// revealer to assert that even when reveal is engaged, a declined token exposes
// neither plaintext nor ciphertext at the egress.
const (
	testPlaintext  = "SUPER-SECRET-PLAINTEXT"
	testCiphertext = "AAAACIPHERTEXTBYTES"
)

// TestMaskDefaultHidesPlaintextAndCiphertext verifies the default egress:
// without reveal, every token becomes the masked placeholder and neither the
// sensitive plaintext nor its ciphertext can appear in the output (Req 16.3,
// 16.7, 22.2, 23.2).
func TestMaskDefaultHidesPlaintextAndCiphertext(t *testing.T) {
	content := "before |encrypt|tok1| middle |encrypt|tok2| after"

	// A revealer that WOULD hand back plaintext/ciphertext, to prove the
	// default path never consults it.
	revealer := func(tokenID string) (string, bool) {
		t.Fatalf("revealer must not be called when reveal is false")
		return testPlaintext, true
	}

	// reveal == false must mask regardless of the revealer.
	got := Mask(content, false, revealer)
	assertMasked(t, got, 2)

	// A nil revealer must also mask, even if reveal is true.
	got = Mask(content, true, nil)
	assertMasked(t, got, 2)
}

// assertMasked checks that output contains exactly wantCount placeholders and
// leaks neither plaintext, ciphertext, nor any residual token.
func assertMasked(t *testing.T, got string, wantCount int) {
	t.Helper()
	if strings.Contains(got, testPlaintext) {
		t.Errorf("masked output leaked plaintext: %q", got)
	}
	if strings.Contains(got, testCiphertext) {
		t.Errorf("masked output leaked ciphertext: %q", got)
	}
	if strings.Contains(got, "|encrypt|") {
		t.Errorf("masked output left a raw token: %q", got)
	}
	if n := strings.Count(got, MaskedPlaceholder); n != wantCount {
		t.Errorf("expected %d masked placeholders, got %d in %q", wantCount, n, got)
	}
}

// TestMaskRevealSubstitutesPlaintextForPermittedTokenOnly verifies that with
// reveal engaged, a token the revealer permits is replaced by its plaintext,
// while a token the revealer declines stays masked and never leaks its
// plaintext or ciphertext (Req 16.4, 16.8).
func TestMaskRevealSubstitutesPlaintextForPermittedTokenOnly(t *testing.T) {
	content := "x |encrypt|permitted| y |encrypt|denied| z"

	revealer := func(tokenID string) (string, bool) {
		if tokenID == "permitted" {
			return testPlaintext, true
		}
		// The denied token: pretend we hold its ciphertext but are not allowed
		// to reveal it. Neither plaintext nor ciphertext may reach the output.
		return "", false
	}

	got := Mask(content, true, revealer)

	if !strings.Contains(got, testPlaintext) {
		t.Errorf("expected permitted token to reveal plaintext, got %q", got)
	}
	if strings.Contains(got, testCiphertext) {
		t.Errorf("output leaked ciphertext: %q", got)
	}
	if strings.Contains(got, "|encrypt|") {
		t.Errorf("output left a raw token: %q", got)
	}
	if n := strings.Count(got, MaskedPlaceholder); n != 1 {
		t.Errorf("expected exactly 1 masked (denied) token, got %d in %q", n, got)
	}
	want := "x " + testPlaintext + " y " + MaskedPlaceholder + " z"
	if got != want {
		t.Errorf("reveal output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestMaskNoTokensReturnsContentUnchanged verifies content without any
// Obfuscation_Token is passed through verbatim in both modes.
func TestMaskNoTokensReturnsContentUnchanged(t *testing.T) {
	content := "plain content with no tokens at all"
	if got := Mask(content, false, nil); got != content {
		t.Errorf("default mask altered token-free content: %q", got)
	}
	rev := func(string) (string, bool) { return testPlaintext, true }
	if got := Mask(content, true, rev); got != content {
		t.Errorf("reveal mask altered token-free content: %q", got)
	}
}

// TestMaskAdjacentTokensDoNotSpan verifies the token regex captures each id up
// to its own closing delimiter and does not swallow a neighboring token.
func TestMaskAdjacentTokensDoNotSpan(t *testing.T) {
	content := "|encrypt|a||encrypt|b|"

	seen := map[string]bool{}
	revealer := func(tokenID string) (string, bool) {
		seen[tokenID] = true
		return "<" + tokenID + ">", true
	}

	got := Mask(content, true, revealer)
	if want := "<a><b>"; got != want {
		t.Errorf("adjacent tokens mismatch: got %q want %q", got, want)
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("both token ids should be resolved individually, saw %v", seen)
	}
}
