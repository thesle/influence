package markdown

import (
	"strings"
	"testing"
)

func TestCommandsCoverAllFourWithIcons(t *testing.T) {
	cmds := Commands()
	if len(cmds) != 4 {
		t.Fatalf("expected 4 @-commands, got %d", len(cmds))
	}
	want := map[AtCommand]CommandKind{
		CmdLink:     EditTime,
		CmdHeading:  EditTime,
		CmdTOC:      RenderTime,
		CmdChildren: RenderTime,
	}
	seen := map[AtCommand]bool{}
	for _, c := range cmds {
		kind, ok := want[c.Command]
		if !ok {
			t.Fatalf("unexpected command %q", c.Command)
		}
		if c.Kind != kind {
			t.Fatalf("command %q kind = %v, want %v", c.Command, c.Kind, kind)
		}
		// Req 11.10: every command must carry an on-screen icon identifier.
		if strings.TrimSpace(c.Icon) == "" {
			t.Fatalf("command %q has no icon identifier", c.Command)
		}
		if strings.TrimSpace(c.Label) == "" {
			t.Fatalf("command %q has no label", c.Command)
		}
		seen[c.Command] = true
	}
	if len(seen) != 4 {
		t.Fatalf("commands were not distinct: %+v", seen)
	}
}

func TestHeadingStylesArePresentedForAllLevels(t *testing.T) {
	styles := HeadingStyles()
	if len(styles) != 6 {
		t.Fatalf("expected 6 heading styles (levels 1-6), got %d", len(styles))
	}
	for i, s := range styles {
		wantLevel := i + 1
		if s.Level != wantLevel {
			t.Fatalf("style %d level = %d, want %d", i, s.Level, wantLevel)
		}
		if s.Prefix != strings.Repeat("#", wantLevel)+" " {
			t.Fatalf("style level %d prefix = %q", wantLevel, s.Prefix)
		}
	}
}

func TestHeadingRendersChosenStyle(t *testing.T) {
	if got := Heading(2, "  Overview  "); got != "## Overview" {
		t.Fatalf("Heading(2, ...) = %q, want %q", got, "## Overview")
	}
	// Out-of-range levels clamp into 1-6 so the result is always valid.
	if got := Heading(0, "Top"); got != "# Top" {
		t.Fatalf("Heading(0, ...) = %q, want %q", got, "# Top")
	}
	if got := Heading(9, "Deep"); got != "###### Deep" {
		t.Fatalf("Heading(9, ...) = %q, want %q", got, "###### Deep")
	}
}

func TestBuildLinkOptionsWithCandidates(t *testing.T) {
	in := []LinkCandidate{
		{ID: "id-1", Name: "Alpha"},
		{ID: "id-2", Name: "Beta"},
	}
	opts := BuildLinkOptions(in)
	if opts.Empty {
		t.Fatalf("expected non-empty options")
	}
	if opts.Indicator != "" {
		t.Fatalf("non-empty options should carry no indicator, got %q", opts.Indicator)
	}
	if len(opts.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(opts.Candidates))
	}
	// Returned slice must be a copy: mutating the input must not change output.
	in[0].Name = "MUTATED"
	if opts.Candidates[0].Name != "Alpha" {
		t.Fatalf("BuildLinkOptions aliased the caller's slice")
	}
}

func TestBuildLinkOptionsEmptyShowsIndicator(t *testing.T) {
	for _, in := range [][]LinkCandidate{nil, {}} {
		opts := BuildLinkOptions(in)
		if !opts.Empty {
			t.Fatalf("expected Empty=true for input %v", in)
		}
		if len(opts.Candidates) != 0 {
			t.Fatalf("expected no candidates, got %d", len(opts.Candidates))
		}
		if strings.TrimSpace(opts.Indicator) == "" {
			t.Fatalf("empty options must carry an indicator (Req 11.3)")
		}
	}
}

func TestLinkMarkdownUsesInternalScheme(t *testing.T) {
	got := LinkMarkdown(LinkCandidate{ID: "abc-123", Name: "My Page"})
	want := "[My Page](influence://polygon/abc-123)"
	if got != want {
		t.Fatalf("LinkMarkdown = %q, want %q", got, want)
	}
}

func TestExpandTOCFromHeadings(t *testing.T) {
	content := []byte("# One\n\nalpha\n\n## Sub A\n\nbeta\n\n# Two\n\ngamma\n")
	got := ExpandTOC(content)
	want := "- One\n" +
		"  - Sub A\n" +
		"- Two\n"
	if got != want {
		t.Fatalf("ExpandTOC =\n%q\nwant\n%q", got, want)
	}
}

func TestExpandTOCNestingRelativeToShallowest(t *testing.T) {
	// Page starts at level 2; indentation should be relative to level 2.
	content := []byte("## Top\n\nx\n\n### Deeper\n\ny\n")
	got := ExpandTOC(content)
	want := "- Top\n  - Deeper\n"
	if got != want {
		t.Fatalf("ExpandTOC =\n%q\nwant\n%q", got, want)
	}
}

func TestExpandTOCEmptyWhenNoHeadings(t *testing.T) {
	if got := ExpandTOC([]byte("just prose\n\nmore prose\n")); got != "" {
		t.Fatalf("expected empty TOC for headingless page, got %q", got)
	}
	if got := ExpandTOC(nil); got != "" {
		t.Fatalf("expected empty TOC for empty content, got %q", got)
	}
}

func TestExpandTOCRegeneratesOnHeadingChange(t *testing.T) {
	before := ExpandTOC([]byte("# A\n\nx\n"))
	after := ExpandTOC([]byte("# A\n\nx\n\n# B\n\ny\n"))
	if before == after {
		t.Fatalf("TOC did not change after heading set changed (Req 11.9)")
	}
	if !strings.Contains(after, "- B") {
		t.Fatalf("regenerated TOC missing new heading: %q", after)
	}
}

func TestExpandChildrenList(t *testing.T) {
	children := []ChildPolygon{
		{ID: "id-1", Name: "Child One"},
		{ID: "id-2", Name: "Child Two"},
	}
	got := ExpandChildren(children)
	want := "- [Child One](influence://polygon/id-1)\n" +
		"- [Child Two](influence://polygon/id-2)\n"
	if got != want {
		t.Fatalf("ExpandChildren =\n%q\nwant\n%q", got, want)
	}
}

func TestExpandChildrenEmpty(t *testing.T) {
	if got := ExpandChildren(nil); got != "" {
		t.Fatalf("expected empty list for no children (Req 11.8), got %q", got)
	}
	if got := ExpandChildren([]ChildPolygon{}); got != "" {
		t.Fatalf("expected empty list for no children (Req 11.8), got %q", got)
	}
}

func TestExpandChildrenRegeneratesOnChildSetChange(t *testing.T) {
	before := ExpandChildren([]ChildPolygon{{ID: "id-1", Name: "One"}})
	after := ExpandChildren([]ChildPolygon{
		{ID: "id-1", Name: "One"},
		{ID: "id-2", Name: "Two"},
	})
	if before == after {
		t.Fatalf("children output did not change after child set changed (Req 11.9)")
	}
	if !strings.Contains(after, "id-2") {
		t.Fatalf("regenerated children list missing new child: %q", after)
	}
}
