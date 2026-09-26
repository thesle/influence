package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// propagationBound is the upper bound Req 25.2 places on collaborative edit
// propagation: a User's change must reach every other User editing the same
// Polygon within 5 seconds. Local fan-out is milliseconds; the bound is a
// worst-case ceiling, so the test asserts delivery comfortably inside it.
const propagationBound = 5 * time.Second

// TestCollaborativePropagationWithinBound stands up a real WebSocket server
// around Hub.Serve and connects two real coder/websocket clients to the same
// per-Polygon collaborative room. It confirms that an edit frame sent by one
// client reaches the other within the 5-second bound (Req 25.2), and that the
// sender does not receive its own frame back (Broadcast excludes the sender).
func TestCollaborativePropagationWithinBound(t *testing.T) {
	hub := NewHub()
	key := testKey("collab-poly")

	// A real HTTP server that upgrades each request to a WebSocket and hands
	// it to the hub for the fixed collaborative room. Every connection joins
	// the same Polygon room, which is what makes the two clients peers.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Serve blocks for the life of the connection; the request context is
		// cancelled when the client disconnects or the server closes.
		hub.Serve(r.Context(), key, ModeCollaborative, conn)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	dialCtx, cancelDial := context.WithTimeout(context.Background(), propagationBound)
	defer cancelDial()

	sender, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial sender: %v", err)
	}
	defer sender.Close(websocket.StatusNormalClosure, "")

	receiver, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial receiver: %v", err)
	}
	defer receiver.Close(websocket.StatusNormalClosure, "")

	// Both clients must be registered in the room before the edit is sent,
	// otherwise the broadcast could race ahead of the receiver's Join. Serve
	// calls Join synchronously on connect, but the Accept handshake completes
	// on the server goroutine, so wait for the room to hold both members.
	waitForRoomSize(t, hub, key, 2)

	// The edit payload is opaque to the transport (a CRDT op would go here);
	// this task exercises propagation, not op semantics.
	edit := []byte(`{"op":"insert","pos":0,"text":"hello"}`)

	sendCtx, cancelSend := context.WithTimeout(context.Background(), propagationBound)
	defer cancelSend()
	if err := sender.Write(sendCtx, websocket.MessageText, edit); err != nil {
		t.Fatalf("send edit: %v", err)
	}

	// Assert the receiver observes the edit within the propagation bound.
	type readResult struct {
		data []byte
		err  error
	}
	got := make(chan readResult, 1)
	go func() {
		// Give the read its own generous deadline; the select below enforces
		// the actual 5-second propagation assertion.
		readCtx, cancel := context.WithTimeout(context.Background(), 2*propagationBound)
		defer cancel()
		_, data, err := receiver.Read(readCtx)
		got <- readResult{data: data, err: err}
	}()

	deadline := time.After(propagationBound)
	select {
	case res := <-got:
		if res.err != nil {
			t.Fatalf("receiver read: %v", res.err)
		}
		if string(res.data) != string(edit) {
			t.Fatalf("propagated edit = %q, want %q", res.data, edit)
		}
	case <-deadline:
		t.Fatalf("edit did not propagate to the other client within %s (Req 25.2)", propagationBound)
	}

	// The sender must not receive its own frame echoed back: Broadcast excludes
	// the sender. A short read deadline is enough — if an echo were coming it
	// would already be here, since the receiver got the edit near-instantly.
	echoCtx, cancelEcho := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelEcho()
	if _, _, err := sender.Read(echoCtx); err == nil {
		t.Fatalf("sender received its own edit back; Broadcast should exclude the sender")
	} else if !isTimeout(err) {
		t.Fatalf("sender read expected a timeout (no echo), got: %v", err)
	}
}

// waitForRoomSize blocks until the room for key holds want clients or a short
// timeout elapses, so the test does not race the asynchronous server-side Join.
func waitForRoomSize(t *testing.T, hub *Hub, key RoomKey, want int) {
	t.Helper()
	deadline := time.Now().Add(propagationBound)
	for time.Now().Before(deadline) {
		if hub.Size(key) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("room did not reach %d members within %s (have %d)", want, propagationBound, hub.Size(key))
}

// isTimeout reports whether err is a deadline-exceeded context error, which is
// how a bounded read that received nothing reports "nothing arrived".
func isTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), context.DeadlineExceeded.Error())
}
