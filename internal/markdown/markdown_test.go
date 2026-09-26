package markdown

import (
	"bytes"
	"strings"
	"testing"
)

// reassemble concatenates the source spans of the given sections, in order, and
// returns the result. If Sections tiles the document correctly this equals the
// original content byte-for-byte.
func reassemble(content []byte, secs []Section) []byte {
	var buf bytes.Buffer
	for _, s := range secs {
		buf.Write(content[s.Start:s.End])
	}
	return buf.Bytes()
}

// assertContiguous checks that sections start at 0, tile without gaps or
// overlaps, and end at len(content).
func assertContiguous(t *testing.T, content []byte, secs []Section) {
	t.Helper()
	if len(secs) == 0 {
		if len(content) != 0 {
			t.Fatalf("no sections returned for %d bytes of content", len(content))
		}
		return
	}
	if secs[0].Start != 0 {
		t.Fatalf("first section starts at %d, want 0", secs[0].Start)
	}
	for i := 1; i < len(secs); i++ {
		if secs[i].Start != secs[i-1].End {
			t.Fatalf("gap/overlap between section %d (end %d) and %d (start %d)",
				i-1, secs[i-1].End, i, secs[i].Start)
		}
	}
	if last := secs[len(secs)-1].End; last != len(content) {
		t.Fatalf("last section ends at %d, want %d", last, len(content))
	}
}

func TestSectionsReconstructOriginalExactly(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"no headings":        "Just a paragraph.\n\nAnd another one.\n",
		"single heading":     "# Title\n\nBody text here.\n",
		"preamble + heading": "Intro paragraph.\n\n# Title\n\nBody.\n",
		"multiple headings": "# One\n\nalpha\n\n## Two\n\nbeta\n\n# Three\n\ngamma\n",
		"trailing blanks":    "# H\n\ncontent\n\n\n",
		"no trailing newline": "# H\n\nno newline at end",
		"setext heading":      "Title\n=====\n\nbody\n\nSub\n---\n\nmore\n",
		"consecutive headings": "# A\n## B\n### C\ntext\n",
		"crlf-ish spacing":     "# H1\n\n\tindented?\n\n# H2\ntail",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			b := []byte(content)
			secs := Sections(b)
			assertContiguous(t, b, secs)
			got := reassemble(b, secs)
			if !bytes.Equal(got, b) {
				t.Fatalf("reassembled bytes differ from original\n got: %q\nwant: %q", got, content)
			}
		})
	}
}

func TestSectionsHeadingMetadata(t *testing.T) {
	content := []byte("Preamble line.\n\n# First\n\nbody\n\n## Second level\n\nmore\n")
	secs := Sections(content)

	// preamble (level 0), then two headings.
	if len(secs) != 3 {
		t.Fatalf("got %d sections, want 3: %+v", len(secs), secs)
	}
	if secs[0].Level != 0 || secs[0].Heading != "" {
		t.Fatalf("section 0 should be preamble (level 0, no heading), got %+v", secs[0])
	}
	if secs[1].Level != 1 || secs[1].Heading != "First" {
		t.Fatalf("section 1 should be level 1 'First', got level %d heading %q", secs[1].Level, secs[1].Heading)
	}
	if secs[2].Level != 2 || secs[2].Heading != "Second level" {
		t.Fatalf("section 2 should be level 2 'Second level', got level %d heading %q", secs[2].Level, secs[2].Heading)
	}

	// Each section's source span must begin at its heading marker.
	if got := string(secs[1].Bytes(content)); !strings.HasPrefix(got, "# First") {
		t.Fatalf("section 1 span should start at its heading marker, got %q", got)
	}
	if got := string(secs[2].Bytes(content)); !strings.HasPrefix(got, "## Second level") {
		t.Fatalf("section 2 span should start at its heading marker, got %q", got)
	}
}

func TestSectionsNoHeadingsIsSinglePreamble(t *testing.T) {
	content := []byte("no headings here\n\njust prose\n")
	secs := Sections(content)
	if len(secs) != 1 {
		t.Fatalf("got %d sections, want 1", len(secs))
	}
	if secs[0].Level != 0 {
		t.Fatalf("lone section should be level-0 preamble, got level %d", secs[0].Level)
	}
	if secs[0].Start != 0 || secs[0].End != len(content) {
		t.Fatalf("preamble should span the whole document, got [%d,%d)", secs[0].Start, secs[0].End)
	}
}

func TestSectionsEmptyInput(t *testing.T) {
	if secs := Sections(nil); secs != nil {
		t.Fatalf("empty input should yield no sections, got %+v", secs)
	}
	if secs := Sections([]byte{}); len(secs) != 0 {
		t.Fatalf("empty input should yield no sections, got %+v", secs)
	}
}

func TestSectionBytesAreVerbatim(t *testing.T) {
	content := []byte("# Alpha\n\nsome *emphasized* text\n\n# Beta\n\ntail\n")
	secs := Sections(content)
	for _, s := range secs {
		if !bytes.Equal(s.Bytes(content), content[s.Start:s.End]) {
			t.Fatalf("Bytes() did not return the raw source span for %+v", s)
		}
	}
}

func TestRenderHTML(t *testing.T) {
	html, err := RenderHTML([]byte("# Title\n\nHello **world**.\n"))
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, "<h1") {
		t.Fatalf("expected an <h1> in rendered HTML, got: %q", got)
	}
	if !strings.Contains(got, "<strong>world</strong>") {
		t.Fatalf("expected bold rendering in HTML, got: %q", got)
	}
}

func TestRenderHTMLEmpty(t *testing.T) {
	html, err := RenderHTML(nil)
	if err != nil {
		t.Fatalf("RenderHTML(nil): %v", err)
	}
	if len(html) != 0 {
		t.Fatalf("empty input should render empty HTML, got %q", string(html))
	}
}

func TestRenderSection(t *testing.T) {
	content := []byte("# One\n\nfirst body\n\n# Two\n\nsecond **body**\n")
	secs := Sections(content)
	// Find the "Two" section and render only it.
	var two Section
	found := false
	for _, s := range secs {
		if s.Heading == "Two" {
			two = s
			found = true
		}
	}
	if !found {
		t.Fatal("did not find the 'Two' section")
	}
	html, err := RenderSection(content, two)
	if err != nil {
		t.Fatalf("RenderSection: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, "second") || !strings.Contains(got, "<strong>body</strong>") {
		t.Fatalf("section render missing expected content, got: %q", got)
	}
	if strings.Contains(got, "first body") {
		t.Fatalf("section render leaked content from another section, got: %q", got)
	}
}
