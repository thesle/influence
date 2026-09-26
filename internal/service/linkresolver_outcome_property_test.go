package service

import (
	"context"
	"testing"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// TestCrossRefractionOutcomeByTargetStateProperty is the property test for
// Cross-Refraction link resolution outcomes keyed on the target's state
// relative to the viewer (Requirements 14.2, 14.3, 14.4).
//
// Feature: influence, Property 11: Cross-Refraction link outcome by target state
//
// Property 11 (from design.md): For all Cross_Refraction links, rendering yields
// an activatable link with the target's current name when the target exists and
// is accessible, a non-activatable "unavailable" link when the target is
// deleted, and a non-activatable "inaccessible" link with the name withheld when
// the target is in a Sphere the viewer cannot access.
//
// The test drives LinkResolver.Resolve over the real file-backed tenant +
// ConnManager + PolicyEngine path (via newLinkResolverFixture) for a target
// placed into exactly one of the three states, then asserts the single matching
// outcome:
//
//   - EXISTS + ACCESSIBLE  → LinkActivatable, Activatable == true, and Name is
//     the target's CURRENT name derived from its content (Req 14.2).
//   - DELETED / UNRESOLVABLE → LinkUnavailable, Activatable == false, and no
//     name is emitted (Req 14.3).
//   - EXISTS BUT INACCESSIBLE → LinkInaccessible, Activatable == false, and the
//     target's name is WITHHELD (empty) (Req 14.4).
//
// In every case the resolver echoes back the canonical target Record_ID (the
// stored reference reveals nothing about content), which the test also checks.
//
// Validates: Requirements 14.2, 14.3, 14.4
func TestCrossRefractionOutcomeByTargetStateProperty(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	// Target names are drawn to be non-empty (so an accessible target resolves
	// to a stable, non-empty heading) and from an alphabet that produces a
	// clean first-heading title. Enough characters/length to keep names varied.
	const nameAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 "

	// The three mutually exclusive target states.
	const (
		stateAccessible = iota
		stateDeleted
		stateInaccessible
	)

	rapid.Check(t, func(rt *rapid.T) {
		// A varied, non-empty target name (its first heading). Constrain to a
		// single line so it becomes the Polygon's derived name verbatim.
		rawName := rapid.StringOfN(rapid.SampledFrom([]rune(nameAlphabet)), 1, 40, -1).Draw(rt, "name")
		// PolygonName trims surrounding whitespace off the heading; mirror that
		// so the expectation matches, and require a non-empty trimmed name.
		wantName := trimName(rawName)
		if wantName == "" {
			rt.Skip("name is whitespace-only after trimming")
		}
		content := "# " + rawName + "\n\nbody"

		state := rapid.SampledFrom([]int{stateAccessible, stateDeleted, stateInaccessible}).Draw(rt, "state")

		// Seed the target's owning Sphere and the target Polygon.
		ownerSphere := seedSphereRow(t, f.db, "owner")
		target := seedPolygonRow(t, f.db, ownerSphere, content)

		// Build authz for the viewer according to the chosen state.
		var authz AuthzData
		switch state {
		case stateAccessible:
			// Grant read access on the target's OWNING Sphere → accessible.
			authz = grantAccess(ownerSphere, data.AccessRead)
		case stateDeleted:
			// Grant on the owning Sphere is irrelevant once the row is gone, but
			// grant it anyway so a mistaken "still resolvable" path would surface
			// as activatable rather than inaccessible.
			authz = grantAccess(ownerSphere, data.AccessRead)
			if _, err := f.db.Exec(`DELETE FROM polygon WHERE record_id = ?`, target); err != nil {
				rt.Fatalf("delete target: %v", err)
			}
		case stateInaccessible:
			// The viewer holds a grant on a DIFFERENT Sphere only; a positive,
			// non-zero offset guarantees it can never collide with the owning
			// Sphere. The target row stays intact so it exists but is unreachable.
			offset := rapid.Int64Range(1, 1_000_000).Draw(rt, "sphereOffset")
			authz = grantAccess(ownerSphere+data.SphereID(offset), data.AccessRead)
		}

		rl, err := f.resolver.Resolve(ctx, f.rc, authz, target)
		if err != nil {
			rt.Fatalf("Resolve(state=%d): unexpected error %v", state, err)
		}

		// The canonical Record_ID is echoed back in every outcome.
		wantID := mustCanonical(rt, target)
		if rl.TargetRecordID != wantID {
			rt.Fatalf("state=%d: TargetRecordID = %q, want %q", state, rl.TargetRecordID, wantID)
		}

		switch state {
		case stateAccessible:
			// Req 14.2: activatable with the target's current name.
			if rl.State != LinkActivatable || !rl.Activatable {
				rt.Fatalf("accessible: state=%v activatable=%v, want activatable+true", rl.State, rl.Activatable)
			}
			if rl.Name != wantName {
				rt.Fatalf("accessible: Name = %q, want %q", rl.Name, wantName)
			}
		case stateDeleted:
			// Req 14.3: non-activatable unavailable, no name.
			if rl.State != LinkUnavailable || rl.Activatable {
				rt.Fatalf("deleted: state=%v activatable=%v, want unavailable+false", rl.State, rl.Activatable)
			}
			if rl.Name != "" {
				rt.Fatalf("deleted: Name = %q, want empty", rl.Name)
			}
		case stateInaccessible:
			// Req 14.4: non-activatable inaccessible, name withheld.
			if rl.State != LinkInaccessible || rl.Activatable {
				rt.Fatalf("inaccessible: state=%v activatable=%v, want inaccessible+false", rl.State, rl.Activatable)
			}
			if rl.Name != "" {
				rt.Fatalf("inaccessible: withheld name expected, got %q", rl.Name)
			}
		}
	})
}

// trimName mirrors PolygonName's heading-trimming so the property's expected
// name matches the resolver's derived name exactly.
func trimName(s string) string {
	return PolygonName("# " + s + "\n")
}

// mustCanonical parses the seeded raw Record_ID and returns its canonical form,
// matching the canonicalization the resolver applies before echoing it back.
func mustCanonical(rt *rapid.T, raw string) string {
	id, err := recordid.Parse(raw)
	if err != nil {
		rt.Fatalf("parse seeded record id %q: %v", raw, err)
	}
	return id.Canonical()
}
