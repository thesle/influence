package ws

import (
	"sync"

	"github.com/influence/influence/internal/data"
)

// RoomKey identifies a single per-Polygon room. A room is scoped to a tenant
// and a Polygon Record_ID together: the tenant is part of the key so two
// tenants that (astronomically unlikely) shared a Record_ID string still get
// distinct rooms, keeping edit/lock signaling structurally within one tenant
// (Req 1.5). Record_ID is the external, immutable Polygon identifier (Req 15.4)
// and never a surrogate id, so keys are stable across renames.
type RoomKey struct {
	TenantID        data.TenantID
	PolygonRecordID string
}

// Client is one connected editor in a room. The hub treats a client as an
// opaque, addressable sink it can broadcast to; the concrete transport (a
// WebSocket connection) is provided elsewhere (conn.go) so the hub stays
// transport-agnostic and testable.
//
// ID must be stable and unique for the lifetime of the connection so the room
// can add and remove exactly this client. Send delivers one message to the
// client; implementations must be safe to call from the hub's broadcast path.
// Send returning an error signals the client is no longer reachable — the hub
// treats such a client as gone but does not itself close the transport.
type Client interface {
	// ID returns a stable, unique identifier for this connection.
	ID() string
	// Send delivers a single message payload to the client.
	Send(msg []byte) error
}

// room is the set of clients editing one Polygon. It is an internal value owned
// entirely by the Hub and guarded by the Hub's lock; it carries no lock of its
// own so all mutation is serialized through the Hub.
type room struct {
	clients map[string]Client
}

func newRoom() *room {
	return &room{clients: make(map[string]Client)}
}

// Hub manages per-Polygon rooms for edit and lock signaling (Req 25.1). It is
// safe for concurrent use: every room mutation and broadcast is serialized
// through a single mutex, which is sufficient for the modest fan-out of a
// small self-hosted deployment and keeps room membership consistent.
//
// The Hub is only the room registry and fan-out mechanism. What a broadcast
// message means — a CRDT operation (Req 25.2) or a lock event (Req 25.3–25.5)
// — is decided by the mode of the Polygon (see ModeResolver) and implemented by
// later tasks.
type Hub struct {
	mu    sync.Mutex
	rooms map[RoomKey]*room
}

// NewHub constructs an empty Hub with no rooms. Rooms are created lazily on the
// first Join for a key and torn down when their last client leaves, so an idle
// Polygon holds no state.
func NewHub() *Hub {
	return &Hub{rooms: make(map[RoomKey]*room)}
}

// Join adds c to the room for key, creating the room if it does not yet exist.
// It returns the number of clients in the room after the join. A client whose
// ID already exists in the room replaces the prior entry (a reconnect under the
// same id), so Join is idempotent with respect to membership count for a given
// id.
func (h *Hub) Join(key RoomKey, c Client) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.rooms[key]
	if !ok {
		r = newRoom()
		h.rooms[key] = r
	}
	r.clients[c.ID()] = c
	return len(r.clients)
}

// Leave removes the client with clientID from the room for key. When the last
// client leaves, the room is deleted so the Hub does not accumulate empty
// rooms. It returns the number of clients remaining in the room; if the room
// (or client) did not exist, it returns 0. Leave is safe to call more than once
// for the same client.
func (h *Hub) Leave(key RoomKey, clientID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.rooms[key]
	if !ok {
		return 0
	}
	delete(r.clients, clientID)
	remaining := len(r.clients)
	if remaining == 0 {
		delete(h.rooms, key)
	}
	return remaining
}

// Broadcast delivers msg to every client currently in the room for key except
// the one identified by exceptID (pass "" to send to all). It is the fan-out
// primitive both concurrency modes use: propagating a collaborative edit to the
// other editors (Req 25.2) or signaling a lock state change to a room
// (Req 25.3–25.5). exceptID lets a sender avoid echoing its own change back.
//
// It returns the number of clients the message was successfully delivered to.
// A client whose Send fails is dropped from the room (its transport is
// considered gone); the caller is responsible for closing that transport. If no
// room exists for key, Broadcast delivers to nobody and returns 0.
func (h *Hub) Broadcast(key RoomKey, exceptID string, msg []byte) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.rooms[key]
	if !ok {
		return 0
	}

	delivered := 0
	var failed []string
	for id, c := range r.clients {
		if id == exceptID {
			continue
		}
		if err := c.Send(msg); err != nil {
			failed = append(failed, id)
			continue
		}
		delivered++
	}
	// Drop clients whose delivery failed; clean up an emptied room.
	for _, id := range failed {
		delete(r.clients, id)
	}
	if len(r.clients) == 0 {
		delete(h.rooms, key)
	}
	return delivered
}

// Size returns the number of clients currently in the room for key, or 0 if no
// room exists. It exists mainly for observability and tests.
func (h *Hub) Size(key RoomKey) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.rooms[key]
	if !ok {
		return 0
	}
	return len(r.clients)
}

// RoomCount returns the number of active (non-empty) rooms the Hub is holding.
func (h *Hub) RoomCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms)
}
