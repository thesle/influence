package service

import (
	"context"
	"errors"
	"testing"

	"github.com/influence/influence/internal/data"
	"pgregory.net/rapid"
)

// TestMinGroupMembershipInvariantProperty is the property test for the minimum
// group-membership invariant (Requirement 4.1).
//
// Feature: influence, Property 2: Minimum group membership invariant
//
// Property 2 (from design.md): For all users and any membership-removal
// operation, if the removal would leave the user with zero Group memberships the
// operation is rejected and memberships are unchanged; otherwise it succeeds and
// the user retains at least one Group.
//
// The test drives arbitrary sequences of add/remove membership operations
// through the real UserGroupService (real ConnManager + in-memory Central +
// tenant, via newUserGroupEnv) against a fixed user and a pool of groups. It
// keeps a shadow model of the expected membership set and, after every
// operation, asserts:
//
//   - a removal that would drop the user to zero groups returns
//     ErrMinGroupMembership and leaves the stored memberships exactly as they
//     were before the attempt;
//   - every other operation (an add, an idempotent add, a removal of a
//     not-held group, or a removal that leaves >=1 group) succeeds; and
//   - once the user has any membership, the stored set never becomes empty.
//
// Validates: Requirements 4.1
func TestMinGroupMembershipInvariantProperty(t *testing.T) {
	conns, central, tenantID, _ := newUserGroupEnv(t)
	ctx := context.Background()
	svc := NewUserGroupService(conns)

	uid := data.UserID(seedUser(t, central, int64(tenantID), "alice", "pw"))

	// A fixed pool of real groups the operations draw from. Using a small pool
	// makes collisions (idempotent adds, removals of not-held groups, removals
	// that reach the last group) frequent across a random sequence.
	const poolSize = 4
	pool := make([]data.GroupID, poolSize)
	for i := range pool {
		pool[i] = seedGroup(t, central, tenantID, "grp-"+string(rune('a'+i)), false)
	}

	// currentMemberships reads the stored membership set for uid as a set keyed
	// by group id, so the test compares against storage, not the shadow model.
	currentMemberships := func(rt *rapid.T) map[data.GroupID]struct{} {
		got, err := svc.Memberships(ctx, uid)
		if err != nil {
			rt.Fatalf("read memberships: %v", err)
		}
		set := make(map[data.GroupID]struct{}, len(got))
		for _, g := range got {
			set[g] = struct{}{}
		}
		return set
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Start each sequence from a clean slate so runs don't accumulate edges.
		if _, err := central.Exec(`DELETE FROM "GROUP_MEMBERSHIP" WHERE user_id = ?`, int64(uid)); err != nil {
			rt.Fatalf("reset memberships: %v", err)
		}

		// Shadow model of the expected membership set.
		expected := make(map[data.GroupID]struct{})

		steps := rapid.IntRange(1, 20).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			idx := rapid.IntRange(0, poolSize-1).Draw(rt, "group")
			g := pool[idx]
			op := rapid.SampledFrom([]string{"add", "remove"}).Draw(rt, "op")

			switch op {
			case "add":
				// Adds only ever grow the set, so they can never violate the
				// invariant (and are idempotent).
				if err := svc.AddMembership(ctx, uid, g); err != nil {
					rt.Fatalf("add membership: %v", err)
				}
				expected[g] = struct{}{}

			case "remove":
				_, held := expected[g]
				before := currentMemberships(rt)
				lastGroup := held && len(expected) == 1

				err := svc.RemoveMembership(ctx, uid, g)

				switch {
				case lastGroup:
					// Removing the user's only group must be rejected and must
					// leave memberships unchanged (Req 4.1).
					if !errors.Is(err, ErrMinGroupMembership) {
						rt.Fatalf("removing last group err = %v, want ErrMinGroupMembership", err)
					}
					after := currentMemberships(rt)
					assertSameSet(rt, before, after)

				default:
					// Removing a not-held group is a no-op success; removing a
					// held group while another remains succeeds. Either way the
					// shadow set drops this group (a no-op delete removes an
					// element that was already absent).
					if err != nil {
						rt.Fatalf("remove membership (held=%v, count=%d) err = %v, want nil", held, len(expected), err)
					}
					delete(expected, g)
				}
			}

			// Storage must match the shadow model after every step.
			assertSetEqualsModel(rt, currentMemberships(rt), expected)

			// The core invariant: once the user has any membership, the stored
			// set is never empty. (Before the first add the user legitimately
			// has zero groups; the service never creates that state itself.)
			if len(expected) > 0 && len(currentMemberships(rt)) == 0 {
				rt.Fatalf("user dropped to zero memberships while %d were expected", len(expected))
			}
		}
	})
}

// assertSameSet fails the property if two membership sets differ.
func assertSameSet(rt *rapid.T, a, b map[data.GroupID]struct{}) {
	if len(a) != len(b) {
		rt.Fatalf("membership set changed: len %d -> %d", len(a), len(b))
	}
	for g := range a {
		if _, ok := b[g]; !ok {
			rt.Fatalf("membership set changed: group %d missing after operation", g)
		}
	}
}

// assertSetEqualsModel fails the property if stored memberships diverge from the
// shadow model.
func assertSetEqualsModel(rt *rapid.T, stored, model map[data.GroupID]struct{}) {
	if len(stored) != len(model) {
		rt.Fatalf("stored memberships (%d) diverge from model (%d)", len(stored), len(model))
	}
	for g := range model {
		if _, ok := stored[g]; !ok {
			rt.Fatalf("model has group %d but storage does not", g)
		}
	}
}
