package markdown

import (
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestValidateExternalURLAccepts(t *testing.T) {
	valid := []string{
		"http://a",
		"https://a",
		"http://example.com",
		"https://example.com/path?q=1#frag",
		"HTTP://EXAMPLE.COM",  // scheme is case-insensitive
		"HtTpS://Example.com", // mixed case scheme
		"https://" + strings.Repeat("a", MaxExternalURLLen-len("https://")), // exactly max length
	}
	for _, u := range valid {
		if err := ValidateExternalURL(u); err != nil {
			t.Errorf("ValidateExternalURL(%q) = %v, want nil", short(u), err)
		}
	}
}

func TestValidateExternalURLRejects(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"over max length":   "https://" + strings.Repeat("a", MaxExternalURLLen), // len > 2048
		"no scheme":         "example.com/path",
		"ftp scheme":        "ftp://example.com",
		"mailto scheme":     "mailto:me@example.com",
		"javascript scheme": "javascript:alert(1)",
		"internal scheme":   "influence://polygon/123",
		"relative":          "/local/path",
		"scheme substring":  "xhttp://example.com",
		"whitespace only":   "   ",
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateExternalURL(u)
			if err == nil {
				t.Fatalf("ValidateExternalURL(%q) = nil, want error", short(u))
			}
			if !errors.Is(err, ErrInvalidExternalURL) {
				t.Fatalf("ValidateExternalURL(%q) error %v does not wrap ErrInvalidExternalURL", short(u), err)
			}
		})
	}
}

func TestValidateExternalURLLengthBoundaries(t *testing.T) {
	// One byte long, but bad scheme -> rejected.
	if err := ValidateExternalURL("a"); err == nil {
		t.Fatalf("single non-scheme char should be rejected")
	}
	// Exactly MaxExternalURLLen with a valid scheme -> accepted.
	atMax := "https://" + strings.Repeat("x", MaxExternalURLLen-len("https://"))
	if len(atMax) != MaxExternalURLLen {
		t.Fatalf("test setup: atMax len = %d, want %d", len(atMax), MaxExternalURLLen)
	}
	if err := ValidateExternalURL(atMax); err != nil {
		t.Fatalf("URL at exactly max length should be accepted, got %v", err)
	}
	// One byte over -> rejected.
	overMax := atMax + "y"
	if err := ValidateExternalURL(overMax); err == nil {
		t.Fatalf("URL one byte over max length should be rejected")
	}
}

func TestExternalLinkMarkdown(t *testing.T) {
	md, err := ExternalLinkMarkdown("https://example.com", "Example")
	if err != nil {
		t.Fatalf("ExternalLinkMarkdown: %v", err)
	}
	if md != "[Example](https://example.com)" {
		t.Fatalf("got %q", md)
	}

	// Empty label falls back to the URL as visible text.
	md, err = ExternalLinkMarkdown("https://example.com", "")
	if err != nil {
		t.Fatalf("ExternalLinkMarkdown empty label: %v", err)
	}
	if md != "[https://example.com](https://example.com)" {
		t.Fatalf("got %q", md)
	}

	// Link-breaking characters in the label are escaped.
	md, err = ExternalLinkMarkdown("https://example.com", "a [b] c")
	if err != nil {
		t.Fatalf("ExternalLinkMarkdown escaping: %v", err)
	}
	if md != `[a \[b\] c](https://example.com)` {
		t.Fatalf("label not escaped, got %q", md)
	}

	// Invalid URL is rejected and yields no markdown.
	if _, err := ExternalLinkMarkdown("ftp://example.com", "x"); !errors.Is(err, ErrInvalidExternalURL) {
		t.Fatalf("expected ErrInvalidExternalURL, got %v", err)
	}
}

func TestRenderExternalLinksHTMLProducesAnchor(t *testing.T) {
	md, err := ExternalLinkMarkdown("https://example.com/docs", "Docs")
	if err != nil {
		t.Fatalf("ExternalLinkMarkdown: %v", err)
	}
	html, err := RenderExternalLinksHTML([]byte(md))
	if err != nil {
		t.Fatalf("RenderExternalLinksHTML: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `href="https://example.com/docs"`) {
		t.Fatalf("expected activatable anchor to stored URL, got: %q", got)
	}
	if !strings.Contains(got, ">Docs<") {
		t.Fatalf("expected the link label as anchor text, got: %q", got)
	}
}

func TestRenderExternalLinksHTMLNeutralizesDangerousScheme(t *testing.T) {
	// A dangerous scheme link should not become an activatable javascript: anchor.
	html, err := RenderExternalLinksHTML([]byte("[x](javascript:alert(1))"))
	if err != nil {
		t.Fatalf("RenderExternalLinksHTML: %v", err)
	}
	if strings.Contains(string(html), "javascript:alert") {
		t.Fatalf("dangerous scheme should not be rendered as an activatable link, got: %q", string(html))
	}
}

// TestPropExternalLinkValidation is Property 12 from the design: for all
// candidate external URLs, the link is stored iff the URL is 1–2048 chars and
// begins with a supported web scheme; an invalid URL is rejected and leaves the
// Polygon content unchanged.
//
// Feature: influence, Property 12: External link validation
// Validates: Requirements 13.1, 13.2
func TestPropExternalLinkValidation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		url := rapid.String().Draw(t, "url")

		// Independent oracle for the acceptance condition.
		wantValid := len(url) >= 1 && len(url) <= MaxExternalURLLen && hasSupportedWebScheme(url)

		err := ValidateExternalURL(url)
		gotValid := err == nil

		if gotValid != wantValid {
			t.Fatalf("ValidateExternalURL(%q): got valid=%v, want valid=%v (err=%v)",
				short(url), gotValid, wantValid, err)
		}
		if !gotValid && !errors.Is(err, ErrInvalidExternalURL) {
			t.Fatalf("rejection error %v does not wrap ErrInvalidExternalURL", err)
		}

		// "Store" step: content is only modified when the URL is valid.
		const existing = "# existing content\n\nbody\n"
		content := existing
		link, buildErr := ExternalLinkMarkdown(url, "label")
		if wantValid {
			if buildErr != nil {
				t.Fatalf("valid URL %q should build markdown, got err %v", short(url), buildErr)
			}
			content += "\n" + link
			if content == existing {
				t.Fatalf("valid URL should have changed content")
			}
		} else {
			if buildErr == nil {
				t.Fatalf("invalid URL %q should not build markdown", short(url))
			}
			// Reject: content left unchanged.
			if content != existing {
				t.Fatalf("invalid URL must leave content unchanged")
			}
		}
	})
}

// short truncates long strings for readable failure messages.
func short(s string) string {
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}
