package ws

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/influence/influence/internal/data"
)

// fakeClient is an in-memory Client for exercising the hub without a real
// WebSocket. It records the messages sent to it and can be told to fail sends
// to simulate a dead transport.
type fakeClient struct {
	id string

	mu       sync.Mutex
	received [][]byte
	failSend bool
}

func newFakeClient(id string) *fakeClient { return &fakeClient{id: id} }

func (c *fakeClient) ID() string { return c.id }

func (c *fakeClient) Send(msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failSend {
		return errors.New("send failed")
	}
	// Copy so callers reusing the buffer cannot mutate recorded history.
	cp := make([]byte, len(msg))
	copy(cp, msg)
	c.received = append(c.received, cp)
	return nil
}

func (c *fakeClient) messages() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.received))
	copy(out, c.received)
	return out
}

func testKey(polygon string) RoomKey {
	return RoomKey{TenantID: 1, PolygonRecordID: polygon}
}

// TestJoinCreatesRoomAndCountsMembers confirms Join lazily creates a room and
// reports the post-join member count (Req 25.1 — per-Polygon rooms).
func TestJoinCreatesRoomAndCountsMembers(t *testing.T) {
	h := NewHub()
	key := testKey("poly-1")

	if got := h.Join(key, newFakeClient("a")); got != 1 {
		t.Fatalf("first join count = %d, want 1", got)
	}
	if got := h.Join(key, newFakeClient("b")); got != 2 {
		t.Fatalf("second join count = %d, want 2", got)
	}
	if got := h.Size(key); got != 2 {
		t.Errorf("room size = %d, want 2", got)
	}
	if got := h.RoomCount(); got != 1 {
		t.Errorf("room count = %d, want 1", got)
	}
}

// TestRoomsAreIsolatedByKey confirms two different Polygons get independent
// rooms and one room's membership never bleeds into another.
func TestRoomsAreIsolatedByKey(t *testing.T) {
	h := NewHub()
	k1 := testKey("poly-1")
	k2 := testKey("poly-2")

	h.Join(k1, newFakeClient("a"))
	h.Join(k2, newFakeClient("b"))

	if h.Size(k1) != 1 || h.Size(k2) != 1 {
		t.Fatalf("sizes = %d,%d, want 1,1", h.Size(k1), h.Size(k2))
	}
	// A different tenant with the same Polygon Record_ID is a different room.
	kOtherTenant := RoomKey{TenantID: 2, PolygonRecordID: "poly-1"}
	if got := h.Size(kOtherTenant); got != 0 {
		t.Errorf("other-tenant same-id room size = %d, want 0", got)
	}
}

// TestJoinSameIDReplaces confirms rejoining under the same client id does not
// double-count membership (a reconnect replaces the prior entry).
func TestJoinSameIDReplaces(t *testing.T) {
	h := NewHub()
	key := testKey("poly-1")

	h.Join(key, newFakeClient("a"))
	if got := h.Join(key, newFakeClient("a")); got != 1 {
		t.Fatalf("rejoin count = %d, want 1", got)
	}
}

// TestLeaveRemovesClientAndTearsDownEmptyRoom confirms Leave decrements
// membership and drops the room once its last client leaves.
func TestLeaveRemovesClientAndTearsDownEmptyRoom(t *testing.T) {
	h := NewHub()
	key := testKey("poly-1")

	h.Join(key, newFakeClient("a"))
	h.Join(key, newFakeClient("b"))

	if got := h.Leave(key, "a"); got != 1 {
		t.Fatalf("leave count = %d, want 1", got)
	}
	if got := h.Leave(key, "b"); got != 0 {
		t.Fatalf("final leave count = %d, want 0", got)
	}
	if got := h.RoomCount(); got != 0 {
		t.Errorf("room count after empty = %d, want 0", got)
	}
	if got := h.Size(key); got != 0 {
		t.Errorf("size of torn-down room = %d, want 0", got)
	}
}

// TestLeaveIsIdempotent confirms leaving a client or room that is not present
// is a no-op returning 0 rather than panicking.
func TestLeaveIsIdempotent(t *testing.T) {
	h := NewHub()
	key := testKey("poly-1")

	if got := h.Leave(key, "ghost"); got != 0 {
		t.Fatalf("leave of missing room = %d, want 0", got)
	}
	h.Join(key, newFakeClient("a"))
	h.Leave(key, "a")
	if got := h.Leave(key, "a"); got != 0 {
		t.Fatalf("double leave = %d, want 0", got)
	}
}

// TestBroadcastReachesOthersExcludingSender confirms a broadcast is delivered
// to every room member except the excluded sender (Req 25.1 fan-out primitive).
func TestBroadcastReachesOthersExcludingSender(t *testing.T) {
	h := NewHub()
	key := testKey("poly-1")

	sender := newFakeClient("sender")
	r1 := newFakeClient("r1")
	r2 := newFakeClient("r2")
	h.Join(key, sender)
	h.Join(key, r1)
	h.Join(key, r2)

	msg := []byte("edit-op")
	if got := h.Broadcast(key, sender.ID(), msg); got != 2 {
		t.Fatalf("delivered = %d, want 2", got)
	}
	if len(sender.messages()) != 0 {
		t.Errorf("sender received its own message; want none")
	}
	for _, c := range []*fakeClient{r1, r2} {
		got := c.messages()
		if len(got) != 1 || string(got[0]) != "edit-op" {
			t.Errorf("client %s messages = %v, want one %q", c.ID(), got, "edit-op")
		}
	}
}

// TestBroadcastToAllWhenNoExclusion confirms passing "" as exceptID delivers to
// every client in the room.
func TestBroadcastToAllWhenNoExclusion(t *testing.T) {
	h := NewHub()
	key := testKey("poly-1")
	a := newFakeClient("a")
	b := newFakeClient("b")
	h.Join(key, a)
	h.Join(key, b)

	if got := h.Broadcast(key, "", []byte("x")); got != 2 {
		t.Fatalf("delivered = %d, want 2", got)
	}
}

// TestBroadcastToMissingRoom confirms broadcasting to a room that does not
// exist delivers to nobody.
func TestBroadcastToMissingRoom(t *testing.T) {
	h := NewHub()
	if got := h.Broadcast(testKey("nope"), "", []byte("x")); got != 0 {
		t.Fatalf("delivered = %d, want 0", got)
	}
}

// TestBroadcastDropsFailingClient confirms a client whose Send fails is removed
// from the room so it is not retried on later broadcasts.
func TestBroadcastDropsFailingClient(t *testing.T) {
	h := NewHub()
	key := testKey("poly-1")

	good := newFakeClient("good")
	bad := newFakeClient("bad")
	bad.failSend = true
	h.Join(key, good)
	h.Join(key, bad)

	if got := h.Broadcast(key, "", []byte("x")); got != 1 {
		t.Fatalf("delivered = %d, want 1 (bad client dropped)", got)
	}
	if got := h.Size(key); got != 1 {
		t.Errorf("room size after drop = %d, want 1", got)
	}
	// The bad client is gone; a second broadcast reaches only the good one.
	if got := h.Broadcast(key, "", []byte("y")); got != 1 {
		t.Errorf("second broadcast delivered = %d, want 1", got)
	}
}

// fakeResolver is an in-memory ModeResolver keyed by Polygon Record_ID.
type fakeResolver struct {
	modes map[string]Mode
	err   error
}

func (r fakeResolver) Mode(_ context.Context, _ data.RequestContext, polygonRecordID string) (Mode, error) {
	if r.err != nil {
		return "", r.err
	}
	m, ok := r.modes[polygonRecordID]
	if !ok {
		return "", errors.New("not found")
	}
	return m, nil
}

// TestResolveModeReturnsPerPolygonMode confirms the hub resolves each Polygon's
// single mode through the resolver (Req 25.1): one Polygon collaborative,
// another record-locking.
func TestResolveModeReturnsPerPolygonMode(t *testing.T) {
	resolver := fakeResolver{modes: map[string]Mode{
		"collab-poly": ModeCollaborative,
		"lock-poly":   ModeLocking,
	}}
	rc := data.NewRequestContext(10, 1, nil, nil)
	ctx := context.Background()

	got, err := ResolveMode(ctx, resolver, rc, testKey("collab-poly"))
	if err != nil {
		t.Fatalf("resolve collaborative: %v", err)
	}
	if got != ModeCollaborative {
		t.Errorf("mode = %q, want %q", got, ModeCollaborative)
	}

	got, err = ResolveMode(ctx, resolver, rc, testKey("lock-poly"))
	if err != nil {
		t.Fatalf("resolve locking: %v", err)
	}
	if got != ModeLocking {
		t.Errorf("mode = %q, want %q", got, ModeLocking)
	}
}

// TestResolveModePropagatesError confirms a resolver error surfaces to the
// caller unchanged (e.g. a Polygon not accessible in this tenant, Req 1.6).
func TestResolveModePropagatesError(t *testing.T) {
	wantErr := errors.New("not accessible")
	resolver := fakeResolver{err: wantErr}
	rc := data.NewRequestContext(10, 1, nil, nil)

	_, err := ResolveMode(context.Background(), resolver, rc, testKey("x"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

// TestModeValid confirms only the two supported modes are valid (Req 25.1).
func TestModeValid(t *testing.T) {
	for _, m := range []Mode{ModeLocking, ModeCollaborative} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	for _, m := range []Mode{Mode(""), Mode("bogus")} {
		if m.Valid() {
			t.Errorf("%q should be invalid", m)
		}
	}
}
