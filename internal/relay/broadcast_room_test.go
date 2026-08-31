package relay

import (
	"encoding/json"
	"testing"
)

// TestBroadcastToRoomReachesAJoinedMember drives connect, join-room, and a
// BroadcastToRoom call from a goroutine that is not a socket handler (the
// production shape: internal/room's background loop calls it through
// internal/app's OnConflictChange wiring), then checks the joined member
// receives it over the wire. This confirms Relay.io.To(room).Emit works
// when called from outside a handler.
func TestBroadcastToRoomReachesAJoinedMember(t *testing.T) {
	rel := New(testConfig(), fakeVerifier{"good-token": {FileID: "room1", UserID: "u1", UserName: "Alice", CanWrite: true}})
	defer rel.Close()

	c := newPollingClient(t, rel.Handler())
	c.connect(`{"token":"good-token"}`)
	c.poll() // CONNECT ack
	c.poll() // init-room

	c.emit("join-room", "room1")
	c.poll() // room-user-change
	c.poll() // user-joined
	c.poll() // sync-designate

	done := make(chan struct{})
	go func() {
		defer close(done)
		rel.BroadcastToRoom("room1", "conflict-state", map[string]bool{"inConflict": true})
	}()
	<-done

	pktType, body := c.poll()
	if pktType != '2' {
		t.Fatalf("packet type = %q, want '2' (EVENT); body=%q", pktType, body)
	}

	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) != 2 {
		t.Fatalf("conflict-state event = %q, want a 2-element array", body)
	}
	var eventName string
	var payload struct {
		InConflict bool `json:"inConflict"`
	}
	_ = json.Unmarshal(raw[0], &eventName)
	_ = json.Unmarshal(raw[1], &payload)
	if eventName != "conflict-state" {
		t.Fatalf("event name = %q, want conflict-state", eventName)
	}
	if !payload.InConflict {
		t.Fatal("payload.inConflict = false, want true")
	}
}

// TestBroadcastToRoomToAnEmptyRoomDoesNotPanic checks the no-listener case:
// a conflict notification for a room nobody has joined yet (or that just
// emptied) must be a silent no-op, not a panic or an error surfaced to the
// caller (BroadcastToRoom returns nothing to check).
func TestBroadcastToRoomToAnEmptyRoomDoesNotPanic(_ *testing.T) {
	rel := New(testConfig(), fakeVerifier{})
	defer rel.Close()

	rel.BroadcastToRoom("no-such-room", "conflict-state", map[string]bool{"inConflict": true})
}
