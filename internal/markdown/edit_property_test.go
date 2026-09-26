package markdown

import (
	"bytes"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Feature: influence, Property 15: Section view→edit byte-for-byte round-trip
//
// Design Property 15 (Validates Requirement 12.3):
//
//	For all Markdown_Page content, rendering a section and then selecting that
//	section for editing without modifying it yields markdown text that is
//	byte-for-byte identical to that section's original stored markdown.
//
// The property is exercised end-to-end against Sections + EditSection +
// ConfirmSectionEdit: for arbitrary markdown content, every section's edit
// buffer (EditSection) must equal content[Start:End] byte-for-byte, and
// confirming with that unmodified buffer (ConfirmSectionEdit) must reproduce
// the full content byte-for-byte.
//
// The generator constrains to the input space that actually shapes sections —
// an optional preamble followed by an arbitrary number of ATX or Setext
// heading-led blocks, with varied bodies, blank runs, and an optionally missing
// final newline — rather than fully random bytes, so most iterations produce
// documents with several non-trivial sections.
func TestSectionViewEditRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		content := genMarkdownContent(t)

		secs := Sections(content)

		for i, s := range secs {
			// View→edit: the edit buffer must be the exact stored bytes of the
			// section's range, sliced directly and never re-serialized.
			buf, err := EditSection(content, i)
			if err != nil {
				t.Fatalf("EditSection(%d): %v", i, err)
			}
			want := content[s.Start:s.End]
			if !bytes.Equal(buf, want) {
				t.Fatalf("section %d edit buffer not byte-for-byte\n got: %q\nwant: %q", i, buf, want)
			}

			// Confirm the unmodified buffer: the full content must be
			// reproduced byte-for-byte, closing the view→edit→confirm loop.
			got, err := ConfirmSectionEdit(content, s, buf)
			if err != nil {
				t.Fatalf("ConfirmSectionEdit(%d): %v", i, err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("confirm-unchanged did not reproduce content for section %d\n got: %q\nwant: %q",
					i, got, content)
			}
		}
	})
}

// genMarkdownContent draws an arbitrary but structurally realistic Markdown_Page
// document: an optional preamble, then zero or more heading-led blocks. It
// biases toward documents that produce several sections so the round-trip is
// tested across meaningful section boundaries rather than trivial one-section
// inputs.
func genMarkdownContent(t *rapid.T) []byte {
	var b strings.Builder

	// Optional preamble (a level-0 section preceding the first heading).
	if rapid.Bool().Draw(t, "hasPreamble") {
		b.WriteString(genBlock(t, "preamble"))
	}

	nBlocks := rapid.IntRange(0, 5).Draw(t, "numHeadingBlocks")
	for i := 0; i < nBlocks; i++ {
		b.WriteString(genHeadingBlock(t, i))
	}

	s := b.String()
	// Occasionally drop the trailing newline to exercise the no-final-newline
	// shape; only meaningful when the doc is non-empty and currently ends in \n.
	if strings.HasSuffix(s, "\n") && rapid.Bool().Draw(t, "dropFinalNewline") {
		s = strings.TrimRight(s, "\n")
	}

	return []byte(s)
}

// genHeadingBlock produces one heading (ATX or Setext) followed by an arbitrary
// body, with a trailing blank run.
func genHeadingBlock(t *rapid.T, i int) string {
	title := rapid.SampledFrom([]string{
		"One", "Two", "Three", "Alpha", "Beta", "Section", "A B C",
	}).Draw(t, "title")

	var b strings.Builder
	if rapid.Bool().Draw(t, "setext") {
		// Setext headings are only level 1 or 2; underline with = or -.
		underline := "====="
		if rapid.Bool().Draw(t, "setextLevel2") {
			underline = "-----"
		}
		b.WriteString(title)
		b.WriteString("\n")
		b.WriteString(underline)
		b.WriteString("\n")
	} else {
		level := rapid.IntRange(1, 6).Draw(t, "atxLevel")
		b.WriteString(strings.Repeat("#", level))
		b.WriteString(" ")
		b.WriteString(title)
		b.WriteString("\n")
	}
	b.WriteString(genBlock(t, "body"))
	return b.String()
}

// genBlock produces a small body: a blank line plus one or more paragraph lines,
// followed by a variable run of trailing blank lines.
func genBlock(t *rapid.T, label string) string {
	var b strings.Builder
	b.WriteString("\n")

	nLines := rapid.IntRange(1, 3).Draw(t, label+"NumLines")
	for j := 0; j < nLines; j++ {
		line := rapid.SampledFrom([]string{
			"alpha", "beta gamma", "some text here", "a", "list-ish - item",
		}).Draw(t, label+"Line")
		b.WriteString(line)
		b.WriteString("\n")
	}

	blanks := rapid.IntRange(0, 2).Draw(t, label+"TrailingBlanks")
	b.WriteString(strings.Repeat("\n", blanks))
	return b.String()
}
