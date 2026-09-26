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

package ws

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// writeTimeout bounds how long a single message write to a client may block, so
// one slow or stalled editor cannot wedge a broadcast to the rest of the room.
const writeTimeout = 5 * time.Second

// wsClient adapts a coder/websocket connection to the hub's Client interface.
// The hub broadcasts to it via Send; the connection is otherwise owned by the
// serve loop that created it. A per-connection write mutex serializes writes
// because the hub may fan a broadcast out from a different goroutine than the
// serve loop, and a WebSocket connection allows only one concurrent writer.
type wsClient struct {
	id   string
	conn *websocket.Conn

	writeMu sync.Mutex
}

// newWSClient wraps conn as a Client with a fresh, unique id.
func newWSClient(conn *websocket.Conn) *wsClient {
	return &wsClient{id: uuid.NewString(), conn: conn}
}

// ID returns the connection's stable unique id.
func (c *wsClient) ID() string { return c.id }

// Send writes one text message to the client, bounded by writeTimeout. It is
// safe to call concurrently with the serve loop's reads.
func (c *wsClient) Send(msg []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return c.conn.Write(ctx, websocket.MessageText, msg)
}

// Serve upgrades r to a WebSocket, registers the connection as a client in the
// per-Polygon room for key, and pumps inbound messages into the hub as
// broadcasts to the rest of the room. It blocks until the client disconnects or
// ctx is done, then leaves the room and closes the connection.
//
// The Polygon's mode is resolved up front via the resolver so the caller (and
// later tasks) know whether this room is collaborative (Req 25.2) or
// record-locking (Req 25.3–25.5); a Polygon not accessible in rc's tenant is
// rejected before any room is joined (Req 1.6). This function provides the
// transport and room wiring only — interpreting message payloads as CRDT ops or
// lock events is the job of tasks 18.4 and 18.2.
func (h *Hub) Serve(ctx context.Context, key RoomKey, mode Mode, conn *websocket.Conn) {
	client := newWSClient(conn)
	h.Join(key, client)
	defer func() {
		h.Leave(key, client.ID())
		// Best-effort normal closure; the connection is already being torn
		// down when Serve returns.
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		typ, msg, err := conn.Read(ctx)
		if err != nil {
			// Any read error (client close, context done, transport failure)
			// ends the session; the deferred Leave removes the client.
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			return
		}
		if typ != websocket.MessageText {
			// Only text edit/lock signaling frames are handled here.
			continue
		}
		// Fan the inbound signal out to the other editors of this Polygon
		// (Req 25.1). The sender is excluded so it does not receive its own
		// message back.
		h.Broadcast(key, client.ID(), msg)
	}
}
