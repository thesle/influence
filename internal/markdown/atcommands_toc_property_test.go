package markdown

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Feature: influence, Property 13: @toc reflects current headings
//
// Property 13 (design.md): For all Markdown_Page contents, the `@toc` output is
// exactly the page's markdown headings in document order, and is empty when the
// page has no headings. Req 11.9 further requires that when the heading set
// changes the TOC regenerates to match.
//
// Validates: Requirements 11.5, 11.6, 11.9

// tocHeading is one heading drawn by the generator, carrying the ground-truth
// level and text so the property has an independent oracle to compare ExpandTOC
// against without re-deriving it from the rendered document.
type tocHeading struct {
	level int
	text  string
}

// buildTOCDoc assembles a Markdown_Page from an ordered list of headings,
// optionally preceded by a preamble paragraph and with body prose after each
// heading. The headings appear in document order, so the list itself is the
// oracle for what @toc must reflect.
func buildTOCDoc(preamble string, heads []tocHeading, bodies []string) []byte {
	var b strings.Builder
	if preamble != "" {
		b.WriteString(preamble)
		b.WriteString("\n\n")
	}
	for i, h := range heads {
		b.WriteString(strings.Repeat("#", h.level))
		b.WriteByte(' ')
		b.WriteString(h.text)
		b.WriteByte('\n')
		if i < len(bodies) && bodies[i] != "" {
			b.WriteString("\n")
			b.WriteString(bodies[i])
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// tocEntries extracts the heading text of each entry ExpandTOC produced, in
// order. Each entry is a "- {heading}" line with leading two-space indentation
// per relative level; stripping the indent and the "- " marker recovers the
// heading text, which is what the entry must reflect.
func tocEntries(toc string) []string {
	if toc == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(toc, "\n"), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		trimmed = strings.TrimPrefix(trimmed, "- ")
		out = append(out, trimmed)
	}
	return out
}

// TestPropTOCReflectsCurrentHeadings is the property-based test for Property 13.
// It draws an arbitrary page shape from an ordered set of headings (levels 1–6)
// plus optional preamble and bodies, then asserts ExpandTOC yields exactly one
// entry per heading in document order — and nothing when there are no headings.
// It then changes the heading set and asserts the TOC regenerates to match the
// new set (Req 11.9).
//
// Validates: Requirements 11.5, 11.6, 11.9
func TestPropTOCReflectsCurrentHeadings(t *testing.T) {
	// A heading-text alphabet with no markdown-special or whitespace-collapsing
	// characters, so the parsed heading text is byte-identical to what we wrote
	// and the oracle stays exact.
	const alphabet = "abcXYZ0129"
	headTextGen := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 1, 12, -1)
	levelGen := rapid.IntRange(1, 6)

	// A generator for an ordered list of headings (possibly empty, so the
	// no-headings case is exercised).
	headsGen := func(t *rapid.T, label string) []tocHeading {
		n := rapid.IntRange(0, 8).Draw(t, label+"Count")
		heads := make([]tocHeading, n)
		for i := 0; i < n; i++ {
			heads[i] = tocHeading{
				level: levelGen.Draw(t, label+"Level"),
				text:  headTextGen.Draw(t, label+"Text"),
			}
		}
		return heads
	}

	// oracle returns the heading texts a correct @toc must list, in order.
	oracle := func(heads []tocHeading) []string {
		if len(heads) == 0 {
			return nil
		}
		want := make([]string, len(heads))
		for i, h := range heads {
			want[i] = h.text
		}
		return want
	}

	equalSeq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	rapid.Check(t, func(t *rapid.T) {
		preamble := rapid.SampledFrom([]string{"", "intro prose", "some\nlines"}).Draw(t, "preamble")

		heads := headsGen(t, "a")
		bodies := make([]string, len(heads))
		for i := range bodies {
			bodies[i] = rapid.SampledFrom([]string{"", "body text", "more\nprose"}).Draw(t, "bodyA")
		}

		content := buildTOCDoc(preamble, heads, bodies)
		toc := ExpandTOC(content)

		// Sanity-check the oracle against the parser: the generator is only a
		// valid oracle if the parser sees exactly the headings we wrote. If the
		// parser disagrees (e.g. it interpreted a body line as a heading), the
		// oracle is the parsed heading set, not our raw list.
		secs := Sections(content)
		var parsedHeadings []string
		for _, s := range secs {
			if s.Level >= 1 {
				parsedHeadings = append(parsedHeadings, s.Heading)
			}
		}

		gotEntries := tocEntries(toc)

		// Property core: the @toc entries are exactly the current headings in
		// document order (Req 11.5), and empty when there are none (Req 11.6).
		if !equalSeq(gotEntries, parsedHeadings) {
			t.Fatalf("@toc entries do not match current headings:\n got=%q\nwant=%q\ncontent=%q",
				gotEntries, parsedHeadings, string(content))
		}
		if len(parsedHeadings) == 0 && toc != "" {
			t.Fatalf("expected empty @toc for headingless page, got %q (content=%q)", toc, string(content))
		}

		// If the crafted document parsed to exactly the headings we wrote (the
		// common case for this alphabet), the raw oracle must also match.
		want := oracle(heads)
		if equalSeq(parsedHeadings, want) && !equalSeq(gotEntries, want) {
			t.Fatalf("@toc entries do not match written headings:\n got=%q\nwant=%q", gotEntries, want)
		}

		// Regeneration (Req 11.9): change the heading set and confirm the TOC
		// tracks the new set rather than the old one.
		heads2 := headsGen(t, "b")
		bodies2 := make([]string, len(heads2))
		for i := range bodies2 {
			bodies2[i] = rapid.SampledFrom([]string{"", "body text", "more\nprose"}).Draw(t, "bodyB")
		}
		content2 := buildTOCDoc(preamble, heads2, bodies2)
		toc2 := ExpandTOC(content2)

		secs2 := Sections(content2)
		var parsedHeadings2 []string
		for _, s := range secs2 {
			if s.Level >= 1 {
				parsedHeadings2 = append(parsedHeadings2, s.Heading)
			}
		}
		gotEntries2 := tocEntries(toc2)
		if !equalSeq(gotEntries2, parsedHeadings2) {
			t.Fatalf("regenerated @toc does not match new headings:\n got=%q\nwant=%q",
				gotEntries2, parsedHeadings2)
		}

		// When the heading sets actually differ, the regenerated TOC must
		// reflect the new set, not the old one.
		if !equalSeq(parsedHeadings, parsedHeadings2) {
			if equalSeq(gotEntries2, parsedHeadings) {
				t.Fatalf("regenerated @toc still reflects the old heading set: got=%q, old=%q, new=%q",
					gotEntries2, parsedHeadings, parsedHeadings2)
			}
		}
	})
}
