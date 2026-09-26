package data

import "testing"

// TestRequestContextAccessors verifies the constructor stores and returns the
// identity fields the middleware attaches (design.md — RequestContext).
func TestRequestContextAccessors(t *testing.T) {
	grants := map[SphereID]SphereGrant{
		1: {Access: AccessWrite, Reveal: true},
		2: {Access: AccessRead, Reveal: false},
	}
	rc := NewRequestContext(42, 7, []GroupID{10, 11}, grants)

	if rc.UserID() != 42 {
		t.Errorf("UserID() = %d, want 42", rc.UserID())
	}
	if rc.TenantID() != 7 {
		t.Errorf("TenantID() = %d, want 7", rc.TenantID())
	}
	if got := rc.Groups(); len(got) != 2 || got[0] != 10 || got[1] != 11 {
		t.Errorf("Groups() = %v, want [10 11]", got)
	}
	g, ok := rc.SphereGrant(1)
	if !ok || g.Access != AccessWrite || !g.Reveal {
		t.Errorf("SphereGrant(1) = %+v, %v; want write+reveal", g, ok)
	}
	if _, ok := rc.SphereGrant(999); ok {
		t.Error("SphereGrant(999) should report no grant")
	}
}

// TestRequestContextIsImmutable asserts that mutating the inputs after
// construction, or the values returned by accessors, cannot change the
// context's authorization state. Downstream services trust RequestContext, so
// it must not be mutable by a holder (design.md).
func TestRequestContextIsImmutable(t *testing.T) {
	groups := []GroupID{1, 2}
	grants := map[SphereID]SphereGrant{5: {Access: AccessRead}}
	rc := NewRequestContext(1, 1, groups, grants)

	// Mutating the caller's inputs after construction must not leak in.
	groups[0] = 999
	grants[5] = SphereGrant{Access: AccessWrite, Reveal: true}
	grants[6] = SphereGrant{Access: AccessWrite}

	if got := rc.Groups(); got[0] != 1 {
		t.Errorf("input mutation leaked into context Groups: %v", got)
	}
	if g, _ := rc.SphereGrant(5); g.Access != AccessRead || g.Reveal {
		t.Errorf("input mutation leaked into context grant: %+v", g)
	}
	if _, ok := rc.SphereGrant(6); ok {
		t.Error("input mutation added a grant to the context")
	}

	// Mutating a returned copy must not affect the context.
	out := rc.Groups()
	out[0] = 777
	if again := rc.Groups(); again[0] != 1 {
		t.Errorf("mutating returned slice leaked into context: %v", again)
	}

	outGrants := rc.SphereGrants()
	outGrants[5] = SphereGrant{Access: AccessWrite}
	outGrants[42] = SphereGrant{Access: AccessWrite}
	if g, _ := rc.SphereGrant(5); g.Access != AccessRead {
		t.Errorf("mutating returned grant map leaked into context: %+v", g)
	}
	if _, ok := rc.SphereGrant(42); ok {
		t.Error("mutating returned grant map added a grant to the context")
	}
}
