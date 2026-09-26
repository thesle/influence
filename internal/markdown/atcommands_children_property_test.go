package markdown

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestPropChildrenReflectsCurrentChildren is Property 14 from the design: for
// all Polygons, the `@children` output is exactly the set of the Polygon's
// direct child Polygons, and is empty when the Polygon has no children;
// changing the child set regenerates the output to match.
//
// The property asserts, for arbitrary child sets, that ExpandChildren yields
// exactly one entry per direct child in the supplied order (empty when none),
// and that changing the child set regenerates the list to match.
//
// Feature: influence, Property 14: @children reflects current children
//
// Validates: Requirements 11.7, 11.8, 11.9
func TestPropChildrenReflectsCurrentChildren(t *testing.T) {
	childGen := rapid.Custom(func(t *rapid.T) ChildPolygon {
		return ChildPolygon{
			ID:   rapid.String().Draw(t, "id"),
			Name: rapid.String().Draw(t, "name"),
		}
	})

	rapid.Check(t, func(t *rapid.T) {
		children := rapid.SliceOf(childGen).Draw(t, "children")

		got := ExpandChildren(children)

		// Empty output exactly when there are no children (Req 11.8).
		if len(children) == 0 {
			if got != "" {
				t.Fatalf("no children must yield empty list, got %q", got)
			}
			return
		}
		if got == "" {
			t.Fatalf("non-empty child set must yield a non-empty list")
		}

		// The output is exactly one entry per direct child, in the supplied
		// order (Req 11.7). Build the expected output independently by
		// concatenating one bulleted internal link per child, in order; a
		// byte-for-byte match proves both the "one entry per child" and the
		// "supplied order" halves of the property. (Constructing the oracle
		// rather than splitting the actual output on newlines keeps it robust
		// to child names that themselves contain newlines.)
		want := childEntries(children)
		if got != want {
			t.Fatalf("children output does not match child set in order:\n got %q\nwant %q",
				got, want)
		}

		// Regeneration on child-set change (Req 11.9): appending a fresh child
		// re-expands to the longer list, byte-for-byte matching the original
		// output followed by the new child's entry.
		extra := childGen.Draw(t, "extra")
		changed := append(append([]ChildPolygon(nil), children...), extra)
		after := ExpandChildren(changed)
		wantAfter := got + childEntries([]ChildPolygon{extra})
		if after != wantAfter {
			t.Fatalf("child-set change did not regenerate list to match:\n got %q\nwant %q",
				after, wantAfter)
		}
	})
}

// childEntries is an independent oracle for ExpandChildren's non-empty output:
// one "- <internal link>\n" bullet per child, in the given order.
func childEntries(children []ChildPolygon) string {
	var b strings.Builder
	for _, c := range children {
		b.WriteString("- ")
		b.WriteString(LinkMarkdown(LinkCandidate{ID: c.ID, Name: c.Name}))
		b.WriteByte('\n')
	}
	return b.String()
}
