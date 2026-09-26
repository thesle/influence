package markdown

// External link support (Req 13). This file is deliberately separate from
// markdown.go so it does not touch the shared goldmark instance's signature:
// validation is a pure function used by callers *before* storing content
// (Req 13.1, 13.2), and anchor rendering is provided by a dedicated goldmark
// instance configured to emit external links as activatable anchors (Req 13.3).
//
// Storage model (design.md): external links are stored inline as ordinary
// CommonMark links, `[label](https://…)`, with the URL constrained to a
// supported web scheme and a bounded length.

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/renderer/html"
)

// MaxExternalURLLen is the inclusive upper bound on the length, in bytes, of a
// stored external URL (Req 13.1, 13.2). URLs must be 1–MaxExternalURLLen bytes.
const MaxExternalURLLen = 2048

// supportedWebSchemes is the set of URL schemes an external link may use
// (design.md Req 13 — web schemes). Only http and https are permitted; other
// schemes (mailto, ftp, javascript, the internal influence:// scheme, …) are
// rejected as invalid external URLs.
var supportedWebSchemes = []string{"http://", "https://"}

// ErrInvalidExternalURL is the distinct sentinel error returned by
// ValidateExternalURL when a candidate URL is empty, exceeds MaxExternalURLLen
// bytes, or does not begin with a supported web scheme (Req 13.2). Callers use
// errors.Is to detect it and surface an "invalid URL" indication while leaving
// the existing Polygon content unchanged.
var ErrInvalidExternalURL = errors.New("markdown: invalid external URL")

// ValidateExternalURL reports whether url is acceptable to store as an external
// link (Req 13.1). A URL is valid iff it is 1–MaxExternalURLLen bytes long and
// begins with a supported web scheme (http:// or https://). On success it
// returns nil; otherwise it returns an error wrapping ErrInvalidExternalURL
// describing why the URL was rejected (Req 13.2).
//
// This function only validates; it does not mutate any content. Callers invoke
// it before storing so that a rejected URL leaves the existing content
// unchanged.
func ValidateExternalURL(url string) error {
	if url == "" {
		return fmt.Errorf("%w: URL is empty", ErrInvalidExternalURL)
	}
	if len(url) > MaxExternalURLLen {
		return fmt.Errorf("%w: URL length %d exceeds maximum of %d",
			ErrInvalidExternalURL, len(url), MaxExternalURLLen)
	}
	if !hasSupportedWebScheme(url) {
		return fmt.Errorf("%w: URL does not begin with a supported web scheme (http:// or https://)",
			ErrInvalidExternalURL)
	}
	return nil
}

// hasSupportedWebScheme reports whether url begins with one of the supported
// web schemes. The scheme prefix comparison is case-insensitive because URL
// schemes are case-insensitive per RFC 3986; the rest of the URL is left as-is.
func hasSupportedWebScheme(url string) bool {
	for _, scheme := range supportedWebSchemes {
		if len(url) >= len(scheme) && strings.EqualFold(url[:len(scheme)], scheme) {
			return true
		}
	}
	return false
}

// ExternalLinkMarkdown builds the stored markdown text for an external link
// from a URL and a human-readable label (Req 13.1). The URL is validated with
// ValidateExternalURL first; an invalid URL yields an error wrapping
// ErrInvalidExternalURL and no markdown, so callers never store an unvalidated
// URL.
//
// The label's link-breaking characters ("[", "]") are escaped so a label
// cannot terminate the link text early or otherwise corrupt the surrounding
// markdown. If the label is empty, the URL itself is used as the visible text.
func ExternalLinkMarkdown(url, label string) (string, error) {
	if err := ValidateExternalURL(url); err != nil {
		return "", err
	}
	text := label
	if text == "" {
		text = url
	}
	return fmt.Sprintf("[%s](%s)", escapeLinkText(text), url), nil
}

// escapeLinkText escapes the characters that would prematurely close or
// otherwise break a markdown link's bracketed text.
func escapeLinkText(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`[`, `\[`,
		`]`, `\]`,
	)
	return r.Replace(s)
}

// externalMD is a goldmark instance configured for rendering stored content
// that contains external links as activatable anchors (Req 13.3). It is kept
// separate from the shared md instance in markdown.go so neither file changes
// the other's configuration.
//
// goldmark's default HTML renderer already emits `[label](https://…)` as an
// `<a href="…">label</a>` and, with unsafe rendering left disabled, filters
// dangerous URL schemes (e.g. javascript:) while preserving http/https links —
// exactly the behavior external-link rendering requires. It is instantiated
// explicitly here to make that guarantee local to external-link rendering.
var externalMD = goldmark.New(
	goldmark.WithRendererOptions(
		// Leave WithUnsafe unset: http/https anchors are still produced, but
		// non-web schemes are neutralized, matching the validated scheme set.
		html.WithXHTML(),
	),
)

// RenderExternalLinksHTML renders stored markdown to display HTML, producing an
// activatable anchor for each stored external link that resolves to the stored
// external URL (Req 13.3). It uses the external-link-configured renderer rather
// than the shared instance.
func RenderExternalLinksHTML(content []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := externalMD.Convert(content, &buf); err != nil {
		return nil, fmt.Errorf("markdown: render external links html: %w", err)
	}
	return buf.Bytes(), nil
}
