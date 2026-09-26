// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

// Package markdown parses stored Markdown_Page content into an AST while
// retaining the exact source byte range of each heading-delimited section, and
// renders markdown (or a single section) to HTML for display.
//
// Storage model (Req 11.1): a Markdown_Page's content is stored as markdown
// text. The editor works section-by-section, where a section is a contiguous
// span of the stored markdown delimited by heading boundaries. To support a
// byte-for-byte view→edit round-trip (Req 12.3), the parser retains the exact
// source byte range [Start,End) of each section so a later step can hand the
// original bytes back verbatim rather than re-serializing a lossy AST.
//
// The rendering direction is AST→HTML for display (Req 12.1); the edit
// direction is served from the retained source offsets, never from the AST.
// This package provides both halves: Sections computes the section offset map,
// and RenderHTML / RenderSection produce display HTML via goldmark.
package markdown

import (
	"bytes"
	"fmt"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// md is the shared goldmark instance used for both parsing and rendering. The
// default configuration is CommonMark; parsing and rendering share one instance
// so the AST a Section is derived from matches what RenderSection renders.
var md = goldmark.New()

// Section is a contiguous span of the stored markdown delimited by heading
// boundaries. Start and End are byte offsets into the original source such that
// source[Start:End] is the exact, verbatim text of the section — including its
// leading heading line and any trailing blank lines up to the next heading.
//
// Concatenating the source spans of the sections returned by Sections, in
// order, reproduces the original content byte-for-byte: sections tile the whole
// document with no gaps or overlaps.
type Section struct {
	// Start is the inclusive byte offset of the section's first byte.
	Start int
	// End is the exclusive byte offset one past the section's last byte.
	End int
	// Level is the heading level (1–6) that opens the section, or 0 for the
	// preamble span that precedes the first heading (if any).
	Level int
	// Heading is the plain-text content of the opening heading, or "" for a
	// level-0 preamble section.
	Heading string
}

// Bytes returns the exact source bytes of this section from source. It performs
// no re-serialization; the returned slice is source[Start:End]. Callers that
// intend to mutate the result should copy it first.
func (s Section) Bytes(source []byte) []byte {
	return source[s.Start:s.End]
}

// Sections parses content and returns the heading-delimited section offset map.
//
// A new section begins at each top-level ATX or Setext heading. Content before
// the first heading (or the whole document, if it has no headings) forms a
// single level-0 preamble section. Sections are contiguous and cover the entire
// input: the first section starts at byte 0, each section ends where the next
// begins, and the final section ends at len(content). This guarantees that
// reassembling the sections' source spans yields content unchanged, which is
// what the byte-for-byte round-trip (Req 12.3) is built on.
//
// An empty input yields no sections.
func Sections(content []byte) []Section {
	if len(content) == 0 {
		return nil
	}

	reader := text.NewReader(content)
	doc := md.Parser().Parse(reader)

	// Collect the byte offset and metadata of every top-level heading. Heading
	// boundaries are the only section delimiters; non-heading blocks belong to
	// whichever heading precedes them.
	type boundary struct {
		offset  int
		level   int
		heading string
	}
	var boundaries []boundary
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		h, ok := n.(*ast.Heading)
		if !ok {
			continue
		}
		start, found := nodeStart(h, content)
		if !found {
			continue
		}
		boundaries = append(boundaries, boundary{
			offset:  start,
			level:   h.Level,
			heading: headingText(h, content),
		})
	}

	var sections []Section

	// Any bytes before the first heading form a level-0 preamble. If the
	// document has no headings at all, the whole document is one preamble
	// section.
	firstHeadingAt := len(content)
	if len(boundaries) > 0 {
		firstHeadingAt = boundaries[0].offset
	}
	if firstHeadingAt > 0 {
		sections = append(sections, Section{Start: 0, End: firstHeadingAt, Level: 0})
	}

	// Each heading opens a section that runs until the next heading (or EOF).
	for i, b := range boundaries {
		end := len(content)
		if i+1 < len(boundaries) {
			end = boundaries[i+1].offset
		}
		sections = append(sections, Section{
			Start:   b.offset,
			End:     end,
			Level:   b.level,
			Heading: b.heading,
		})
	}

	return sections
}

// nodeStart returns the byte offset of the first source segment belonging to a
// block node's subtree. Heading text lives in the heading's inline children, so
// the block heading node itself carries no lines; we descend to the first leaf
// that has a source segment. The reported offset is walked back over the ATX
// marker ("#" characters and the space) and any Setext content so the section
// begins at the true first byte of the heading construct.
func nodeStart(n ast.Node, source []byte) (int, bool) {
	seg, ok := firstSegment(n)
	if !ok {
		return 0, false
	}
	start := seg.Start
	// Walk back to the beginning of the line so the "# " ATX marker (or the
	// full Setext heading line) is included in the section span. Everything
	// from the line start up to the heading text is part of the heading.
	for start > 0 && source[start-1] != '\n' {
		start--
	}
	return start, true
}

// firstSegment finds the earliest source text segment in a node's subtree,
// returning the segment with the smallest Start offset. Block nodes such as
// Heading hold their text in inline descendants, so a plain Lines() call on the
// block yields nothing; a depth-first scan locates the real source position.
func firstSegment(n ast.Node) (text.Segment, bool) {
	var best text.Segment
	found := false

	consider := func(seg text.Segment) {
		if !found || seg.Start < best.Start {
			best = seg
			found = true
		}
	}

	if t, ok := n.(*ast.Text); ok {
		consider(t.Segment)
	}
	// Lines() is only valid on block nodes; calling it on an inline node
	// panics, so guard on the node kind before touching it.
	if n.Type() == ast.TypeBlock {
		if lines := n.Lines(); lines != nil {
			for i := 0; i < lines.Len(); i++ {
				consider(lines.At(i))
			}
		}
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if seg, ok := firstSegment(c); ok {
			consider(seg)
		}
	}
	return best, found
}

// headingText returns the plain-text content of a heading by concatenating the
// source of its text segments. It deliberately drops inline markup so the
// result is suitable as a display/label string (for example, a table-of-
// contents entry), not for re-serialization.
func headingText(h *ast.Heading, source []byte) string {
	var buf bytes.Buffer
	for c := h.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			buf.Write(t.Segment.Value(source))
		} else if seg, ok := firstSegment(c); ok {
			// Fallback for inline nodes (emphasis, code spans) that nest their
			// text: use the raw source span so no visible text is lost.
			end := segmentSubtreeEnd(c)
			if end > seg.Start && end <= len(source) {
				buf.Write(source[seg.Start:end])
			}
		}
	}
	return buf.String()
}

// segmentSubtreeEnd returns the largest source segment End offset within a
// node's subtree, used to bound a raw-source slice for inline nodes.
func segmentSubtreeEnd(n ast.Node) int {
	end := 0
	if t, ok := n.(*ast.Text); ok && t.Segment.Stop > end {
		end = t.Segment.Stop
	}
	if n.Type() == ast.TypeBlock {
		if lines := n.Lines(); lines != nil {
			for i := 0; i < lines.Len(); i++ {
				if s := lines.At(i).Stop; s > end {
					end = s
				}
			}
		}
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if e := segmentSubtreeEnd(c); e > end {
			end = e
		}
	}
	return end
}

// RenderHTML renders the full markdown content to display HTML via goldmark
// (Req 12.1). It is the AST→HTML display direction; it is not used to source the
// edit buffer.
func RenderHTML(content []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := md.Convert(content, &buf); err != nil {
		return nil, fmt.Errorf("markdown: render html: %w", err)
	}
	return buf.Bytes(), nil
}

// RenderSection renders a single section's markdown to display HTML (Req 12.1).
// The section's original source bytes are converted independently so the
// rendered fragment reflects exactly that section's stored markdown.
func RenderSection(content []byte, s Section) ([]byte, error) {
	return RenderHTML(s.Bytes(content))
}
