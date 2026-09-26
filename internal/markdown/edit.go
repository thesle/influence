package markdown

import "fmt"

// This file implements the "view→edit" half of the section round-trip
// (Req 12.2, 12.3, 12.4). The rendering direction (AST→HTML) lives in
// markdown.go; here we serve the edit buffer straight from the retained source
// offsets and never re-serialize from the (lossy) AST.
//
// The three moves an editor makes on a section are modeled as pure functions:
//
//   EditSection        — open a section: hand back its EXACT stored bytes as
//                        the edit buffer (Req 12.2, 12.3).
//   ConfirmSectionEdit — confirm a change: splice the new bytes back into the
//                        full content, replacing only that section's range.
//   CancelSectionEdit  — cancel: discard the buffer and leave stored bytes
//                        untouched (Req 12.4). Because stored content is only
//                        ever mutated by an explicit ConfirmSectionEdit, cancel
//                        is simply "return the stored content unchanged".

// EditSection returns the edit buffer for the section at the given index in
// Sections(content): the exact, verbatim stored bytes of that section's range
// (Req 12.2, 12.3). The buffer is a copy, so the caller may mutate it freely
// without touching the stored content; discarding it (cancel) leaves the stored
// bytes untouched (Req 12.4).
//
// The bytes are sliced directly from content[Start:End] — they are never
// re-serialized from the AST — so opening a section for editing and reading it
// back is byte-for-byte identical to the original stored markdown.
//
// An index outside the range of sections yields an error.
func EditSection(content []byte, index int) ([]byte, error) {
	secs := Sections(content)
	if index < 0 || index >= len(secs) {
		return nil, fmt.Errorf("markdown: edit section: index %d out of range [0,%d)", index, len(secs))
	}
	return editBuffer(content, secs[index]), nil
}

// EditSectionAtOffset returns the edit buffer for the section whose range
// begins at the given byte offset (typically a heading's start offset recorded
// when the section was rendered). It returns the resolved Section alongside the
// verbatim buffer so a later ConfirmSectionEdit can target the same range.
//
// The returned buffer is the exact stored bytes for the section (Req 12.2,
// 12.3), copied so the caller can edit or discard it without affecting stored
// content (Req 12.4). An offset that does not start a section yields an error.
func EditSectionAtOffset(content []byte, offset int) ([]byte, Section, error) {
	for _, s := range Sections(content) {
		if s.Start == offset {
			return editBuffer(content, s), s, nil
		}
	}
	return nil, Section{}, fmt.Errorf("markdown: edit section: no section starts at offset %d", offset)
}

// ConfirmSectionEdit produces the new full content by replacing the section's
// original range [s.Start,s.End) with newBytes, leaving every other byte of
// content untouched. This is the only operation that changes stored content;
// until it is called the stored bytes are unchanged (which is what makes cancel
// a no-op — see CancelSectionEdit, Req 12.4).
//
// Passing newBytes equal to the section's original bytes reproduces content
// byte-for-byte, closing the view→edit→confirm round-trip (Req 12.3). The result
// is a freshly allocated slice; content is not mutated.
//
// An out-of-range section yields an error rather than a corrupt splice.
func ConfirmSectionEdit(content []byte, s Section, newBytes []byte) ([]byte, error) {
	if s.Start < 0 || s.End < s.Start || s.End > len(content) {
		return nil, fmt.Errorf("markdown: confirm edit: section range [%d,%d) invalid for %d bytes",
			s.Start, s.End, len(content))
	}
	out := make([]byte, 0, s.Start+len(newBytes)+(len(content)-s.End))
	out = append(out, content[:s.Start]...)
	out = append(out, newBytes...)
	out = append(out, content[s.End:]...)
	return out, nil
}

// CancelSectionEdit models cancelling a section edit: the edit buffer is
// discarded and the stored content is returned unchanged (Req 12.4). It exists
// to make the cancel semantics explicit and testable; because EditSection hands
// out a copy and ConfirmSectionEdit is the only mutator, cancelling can never
// affect stored bytes.
func CancelSectionEdit(content []byte) []byte {
	return content
}

// editBuffer returns a copy of the section's verbatim source span. Copying
// isolates the edit buffer from the stored content so mutating (or discarding)
// it cannot alter stored bytes.
func editBuffer(content []byte, s Section) []byte {
	src := s.Bytes(content)
	buf := make([]byte, len(src))
	copy(buf, src)
	return buf
}
