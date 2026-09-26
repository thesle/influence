package service

import (
	"context"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/influence/influence/internal/data"
)

// Feature: influence, Property 10: ID-stable link resolution
//
// Property 10 (design.md): For all Polygons targeted by links and any rename of
// a target, every link continues to reference the same Record_ID and, when the
// target exists and is accessible, resolves to the target's current (renamed)
// display name.
//
// Validates: Requirements 14.2, 15.1, 15.3
//
// This exercises the whole invariant on the production path (real ConnManager +
// PolicyEngine via newLinkResolverFixture): a Cross-Refraction link is STORED by
// the target's Record_ID (`[label](influence://polygon/{record_id})`, Req 15.1),
// never by name. For an accessible target ResolveContent resolves that stored
// Record_ID to the target's CURRENT name (Req 14.2, 15.3). Renaming the target —
// editing its first heading — leaves the Record_ID (and therefore the stored
// link) untouched; the SAME stored link then resolves to the updated name
// (Req 15.3). The rapid default of 100 iterations satisfies the "minimum 100
// iterations" requirement; each iteration draws a fresh title, an arbitrary new
// title, an author label, and surrounding prose.
func TestPropIDStableLinkResolution(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	// A heading-safe alphabet: letters, digits, and interior spaces only. It
	// omits markdown-structural or whitespace-collapsing runes ('#', '[', ']',
	// '(', ')', '\n', punctuation) so the heading text the parser recovers is
	// byte-identical to what we wrote and PolygonName returns it verbatim,
	// keeping the oracle exact (the same constraint the @toc property uses).
	// The first rune is a non-space and trailing spaces are trimmed so the
	// derived name equals the drawn string after PolygonName's TrimSpace.
	const alphabet = "abcXYZ0129 "
	nameGen := rapid.Custom(func(rt *rapid.T) string {
		head := rapid.SampledFrom([]rune("abcXYZ0129")).Draw(rt, "head")
		rest := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 0, 60, -1).Draw(rt, "rest")
		// Collapse is not a concern for single interior spaces, but trailing
		// spaces would be stripped by PolygonName's TrimSpace, so trim here to
		// keep the expectation exact.
		return strings.TrimRight(string(head)+rest, " ")
	})

	rapid.Check(t, func(rt *rapid.T) {
		initialName := nameGen.Draw(rt, "initialName")
		renamedName := nameGen.Draw(rt, "renamedName")
		// The author's label is arbitrary and deliberately independent of the
		// target's name — resolution must ignore it in favor of the current
		// name. Allow the label to be empty too.
		label := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 0, 30, -1).Draw(rt, "label")
		prefix := rapid.SampledFrom([]string{"", "see ", "refer to "}).Draw(rt, "prefix")
		suffix := rapid.SampledFrom([]string{"", " for details", "."}).Draw(rt, "suffix")

		// Seed a fresh, accessible target for this iteration. Fresh Record_IDs
		// are globally unique, so iterations never collide even though rows
		// accumulate in the shared tenant DB.
		sphere := seedSphereRow(t, f.db, "S")
		target := seedPolygonRow(t, f.db, sphere, "# "+initialName+"\n\nbody")
		authz := grantAccess(sphere, data.AccessRead)

		// The stored content references the target purely by Record_ID.
		content := prefix + linkMarkdown(label, target) + suffix

		// Before rename: the accessible target resolves to its CURRENT name,
		// keeping the stored Record_ID URL (Req 14.2, 15.1, 15.3).
		wantBefore := prefix + linkMarkdown(initialName, target) + suffix
		before, err := f.resolver.ResolveContent(ctx, f.rc, authz, content)
		if err != nil {
			rt.Fatalf("ResolveContent before rename: %v", err)
		}
		if before != wantBefore {
			rt.Fatalf("before rename resolved = %q, want %q", before, wantBefore)
		}

		// Rename the target by editing its first heading only. The Record_ID is
		// unchanged, so the stored link is byte-identical; only the derived name
		// differs (Req 15.1, 15.3).
		if _, err := f.db.Exec(
			`UPDATE polygon SET content = ? WHERE record_id = ?`,
			"# "+renamedName+"\n\nbody", target,
		); err != nil {
			rt.Fatalf("rename target: %v", err)
		}

		// After rename: the SAME stored link (same Record_ID, same input
		// content) now resolves to the UPDATED name, and the Record_ID URL is
		// preserved so the reference itself is unchanged (Req 15.1, 15.3).
		wantAfter := prefix + linkMarkdown(renamedName, target) + suffix
		after, err := f.resolver.ResolveContent(ctx, f.rc, authz, content)
		if err != nil {
			rt.Fatalf("ResolveContent after rename: %v", err)
		}
		if after != wantAfter {
			rt.Fatalf("after rename resolved = %q, want %q (initial=%q renamed=%q)",
				after, wantAfter, initialName, renamedName)
		}
		if !strings.Contains(after, "influence://polygon/"+target) {
			rt.Fatalf("rename must preserve the stored Record_ID reference: %q", after)
		}
	})
}
