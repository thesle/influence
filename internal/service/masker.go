package service

// Centralized masking at every egress (design § Masking at every egress,
// Requirements 16.3, 16.7, 22.2, 23.2).
//
// Stored POLYGON.content never holds sensitive plaintext; a sensitive span is
// replaced at encrypt time by an Obfuscation_Token of the form
// `|encrypt|{id}|` (Req 16.2). Everything that turns stored content into
// something a user (or a search index, or a PDF) can observe MUST route through
// this single Masker so the "plaintext never leaks" property is testable at one
// seam rather than re-implemented per feature.
//
// The Masker is deliberately a pure string→string transform: given the stored
// content and a `reveal` flag derived from the request context, it rewrites
// every token. By default (reveal == false, or a nil revealer) every token
// becomes a masked placeholder and NEITHER the plaintext NOR the ciphertext
// appears in the output (Req 16.3). When reveal is permitted, a token is
// replaced with the plaintext supplied by the revealer callback; if the
// revealer declines a token (missing/undecryptable ciphertext, or per-token
// permission not granted), that token stays masked so no partial plaintext
// escapes (Req 16.8).
//
// The actual per-token decryption and the per-Sphere permission checks live in
// task 12.4 (reveal); the Masker only owns the token-substitution seam and
// invokes the revealer callback. Render, PDF export, and search-index
// population all call Mask:
//   - Render: masked unless the viewer has per-token reveal.
//   - PDF export: produced from the app's rendered HTML, so it inherits masking
//     (Req 22.2).
//   - Search index: the FTS row is built from masked content, so plaintext is
//     never indexed and cannot match or surface for a non-reveal context
//     (Req 16.7, 23.2). Ciphertext is likewise excluded.

import "regexp"

// MaskedPlaceholder is the text substituted for an Obfuscation_Token whenever it
// is not revealed. It contains neither plaintext nor ciphertext (Req 16.3).
const MaskedPlaceholder = "🔒 hidden"

// obfuscationTokenPattern matches a stored Obfuscation_Token `|encrypt|{id}|`
// and captures its {id}. The id is one or more characters that are not a `|`,
// so tokens are matched greedily up to their closing delimiter without spanning
// into an adjacent token. This is the canonical token format from Req 16.2; it
// is defined here in the masking seam that every egress shares.
var obfuscationTokenPattern = regexp.MustCompile(`\|encrypt\|([^|]+)\|`)

// Revealer resolves an Obfuscation_Token id to its plaintext. It returns
// (plaintext, true) when the token may be revealed for the current context and
// (\"\", false) otherwise — the latter covering both "not permitted" and
// "ciphertext missing or undecryptable" so the Masker can keep the token masked
// without learning why (Req 16.5, 16.8). The concrete implementation (per-token
// decryption plus per-Sphere permission check) is supplied by task 12.4.
type Revealer func(tokenID string) (plaintext string, ok bool)

// Mask transforms stored content containing Obfuscation_Tokens into output for a
// single egress (render, PDF export, or search-index population). It is the sole
// component that turns tokens into observable output.
//
// When reveal is false, or revealer is nil, every token is replaced with
// MaskedPlaceholder; neither the plaintext nor the ciphertext appears in the
// result (Req 16.3, 16.7, 22.2, 23.2). When reveal is true and revealer is
// non-nil, each token is offered to revealer: a token the revealer permits is
// replaced with its plaintext, and any token the revealer declines stays masked
// so no partial plaintext leaks (Req 16.8). Content with no tokens is returned
// unchanged.
func Mask(content string, reveal bool, revealer Revealer) string {
	if !reveal || revealer == nil {
		// Fast, unconditional masking path: no revealer is ever consulted, so
		// there is no way for plaintext or ciphertext to reach the output.
		return obfuscationTokenPattern.ReplaceAllString(content, MaskedPlaceholder)
	}
	return obfuscationTokenPattern.ReplaceAllStringFunc(content, func(match string) string {
		sub := obfuscationTokenPattern.FindStringSubmatch(match)
		if len(sub) != 2 {
			// Defensive: a match without a captured id cannot be revealed.
			return MaskedPlaceholder
		}
		tokenID := sub[1]
		if plaintext, ok := revealer(tokenID); ok {
			return plaintext
		}
		return MaskedPlaceholder
	})
}
