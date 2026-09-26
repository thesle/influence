package markdown

import (
	"bytes"
	"testing"
)

// editRoundTripDocs are representative Markdown_Page contents that exercise
// preamble, ATX and Setext headings, trailing blanks, and a missing final
// newline — the same shapes the section parser is proven against.
var editRoundTripDocs = map[string]string{
	"no headings":         "Just a paragraph.\n\nAnd another one.\n",
	"single heading":      "# Title\n\nBody text here.\n",
	"preamble + heading":  "Intro paragraph.\n\n# Title\n\nBody.\n",
	"multiple headings":   "# One\n\nalpha\n\n## Two\n\nbeta\n\n# Three\n\ngamma\n",
	"trailing blanks":     "# H\n\ncontent\n\n\n",
	"no trailing newline": "# H\n\nno newline at end",
	"setext heading":      "Title\n=====\n\nbody\n\nSub\n---\n\nmore\n",
}

// TestEditSectionReturnsVerbatimBytes asserts the edit buffer for every section
// is byte-for-byte identical to that section's stored source span, sliced
// directly from the stored content and never re-serialized (Req 12.2, 12.3).
func TestEditSectionReturnsVerbatimBytes(t *testing.T) {
	for name, doc := range editRoundTripDocs {
		t.Run(name, func(t *testing.T) {
			content := []byte(doc)
			secs := Sections(content)
			for i, s := range secs {
				buf, err := EditSection(content, i)
				if err != nil {
					t.Fatalf("EditSection(%d): %v", i, err)
				}
				want := content[s.Start:s.End]
				if !bytes.Equal(buf, want) {
					t.Fatalf("section %d edit buffer not verbatim\n got: %q\nwant: %q", i, buf, want)
				}
			}
		})
	}
}

// TestEditSectionBufferIsIsolatedCopy verifies the returned buffer is a copy:
// mutating it must not change the stored content, so a subsequent cancel leaves
// stored bytes untouched (Req 12.4).
func TestEditSectionBufferIsIsolatedCopy(t *testing.T) {
	content := []byte("# One\n\nalpha\n\n# Two\n\nbeta\n")
	original := append([]byte(nil), content...)

	buf, err := EditSection(content, 0)
	if err != nil {
		t.Fatalf("EditSection: %v", err)
	}
	for i := range buf {
		buf[i] = 'X'
	}
	if !bytes.Equal(content, original) {
		t.Fatalf("mutating the edit buffer altered stored content\n got: %q\nwant: %q", content, original)
	}
}

// TestEditSectionAtOffsetResolvesByStart checks that resolving a section by its
// start offset yields the same verbatim buffer and the matching Section.
func TestEditSectionAtOffsetResolvesByStart(t *testing.T) {
	content := []byte("intro\n\n# One\n\nalpha\n\n## Two\n\nbeta\n")
	secs := Sections(content)
	for _, want := range secs {
		buf, got, err := EditSectionAtOffset(content, want.Start)
		if err != nil {
			t.Fatalf("EditSectionAtOffset(%d): %v", want.Start, err)
		}
		if got != want {
			t.Fatalf("resolved section mismatch: got %+v want %+v", got, want)
		}
		if !bytes.Equal(buf, content[want.Start:want.End]) {
			t.Fatalf("offset %d buffer not verbatim: got %q", want.Start, buf)
		}
	}

	if _, _, err := EditSectionAtOffset(content, 1); err == nil {
		t.Fatal("offset that does not start a section should error")
	}
}

// TestEditSectionIndexOutOfRange ensures invalid indices are reported.
func TestEditSectionIndexOutOfRange(t *testing.T) {
	content := []byte("# H\n\nbody\n")
	if _, err := EditSection(content, -1); err == nil {
		t.Fatal("negative index should error")
	}
	if _, err := EditSection(content, len(Sections(content))); err == nil {
		t.Fatal("index past last section should error")
	}
}

// TestConfirmUnchangedReproducesContent closes the view→edit→confirm round-trip:
// confirming with the unmodified edit buffer must reproduce the full content
// byte-for-byte (Req 12.3).
func TestConfirmUnchangedReproducesContent(t *testing.T) {
	for name, doc := range editRoundTripDocs {
		t.Run(name, func(t *testing.T) {
			content := []byte(doc)
			secs := Sections(content)
			for i, s := range secs {
				buf, err := EditSection(content, i)
				if err != nil {
					t.Fatalf("EditSection(%d): %v", i, err)
				}
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
}

// TestConfirmReplacesOnlyTargetSection verifies a real edit changes exactly the
// target section and leaves the surrounding bytes untouched.
func TestConfirmReplacesOnlyTargetSection(t *testing.T) {
	content := []byte("# One\n\nalpha\n\n# Two\n\nbeta\n")
	secs := Sections(content)
	// Edit the second section ("Two").
	var two Section
	found := false
	for _, s := range secs {
		if s.Heading == "Two" {
			two = s
			found = true
		}
	}
	if !found {
		t.Fatal("did not find 'Two' section")
	}

	replacement := []byte("# Two\n\nBETA CHANGED\n")
	got, err := ConfirmSectionEdit(content, two, replacement)
	if err != nil {
		t.Fatalf("ConfirmSectionEdit: %v", err)
	}
	want := []byte("# One\n\nalpha\n\n# Two\n\nBETA CHANGED\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("confirm spliced incorrectly\n got: %q\nwant: %q", got, want)
	}
	// The first section's bytes must be present unchanged.
	if !bytes.HasPrefix(got, []byte("# One\n\nalpha\n\n")) {
		t.Fatalf("confirm altered an unrelated section: %q", got)
	}
}

// TestConfirmDoesNotMutateStoredContent ensures ConfirmSectionEdit returns a new
// slice and never mutates the passed-in stored content.
func TestConfirmDoesNotMutateStoredContent(t *testing.T) {
	content := []byte("# One\n\nalpha\n\n# Two\n\nbeta\n")
	original := append([]byte(nil), content...)
	secs := Sections(content)

	if _, err := ConfirmSectionEdit(content, secs[0], []byte("# One\n\nCHANGED\n")); err != nil {
		t.Fatalf("ConfirmSectionEdit: %v", err)
	}
	if !bytes.Equal(content, original) {
		t.Fatalf("ConfirmSectionEdit mutated stored content\n got: %q\nwant: %q", content, original)
	}
}

// TestCancelLeavesStoredBytesUntouched captures the cancel semantics: opening a
// section for editing, then cancelling, leaves the stored content byte-for-byte
// unchanged (Req 12.4). Even mutating the discarded edit buffer has no effect.
func TestCancelLeavesStoredBytesUntouched(t *testing.T) {
	content := []byte("# One\n\nalpha\n\n# Two\n\nbeta\n")
	original := append([]byte(nil), content...)

	buf, err := EditSection(content, 1)
	if err != nil {
		t.Fatalf("EditSection: %v", err)
	}
	// User types into the buffer then cancels — buffer is discarded.
	for i := range buf {
		buf[i] = '?'
	}
	stored := CancelSectionEdit(content)
	if !bytes.Equal(stored, original) {
		t.Fatalf("cancel changed stored content\n got: %q\nwant: %q", stored, original)
	}
}

// TestConfirmInvalidRange rejects malformed section ranges.
func TestConfirmInvalidRange(t *testing.T) {
	content := []byte("# H\n\nbody\n")
	if _, err := ConfirmSectionEdit(content, Section{Start: -1, End: 2}, nil); err == nil {
		t.Fatal("negative start should error")
	}
	if _, err := ConfirmSectionEdit(content, Section{Start: 2, End: 1}, nil); err == nil {
		t.Fatal("end before start should error")
	}
	if _, err := ConfirmSectionEdit(content, Section{Start: 0, End: len(content) + 1}, nil); err == nil {
		t.Fatal("end past content should error")
	}
}
