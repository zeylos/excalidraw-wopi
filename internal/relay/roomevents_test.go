package relay

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	socket "github.com/zishang520/socket.io/servers/socket/v3"
)

// gatedRoomEvents is a RoomEvents whose OnJoin blocks for delay only when
// the joining user is slowUserID, so a test can hold room1's emit lock
// (relay.go's roomEmitLocks) for a known duration without also delaying
// every other joiner's own hook call. started closes once the slow OnJoin
// call has begun (and so is already holding the lock), so a test can wait
// for that before it starts timing a second, contending join.
type gatedRoomEvents struct {
	slowUserID string
	delay      time.Duration
	started    chan struct{}
}

func (e *gatedRoomEvents) OnJoin(_, userID string, _ bool) {
	if userID == e.slowUserID {
		close(e.started)
		time.Sleep(e.delay)
	}
}

func (e *gatedRoomEvents) OnLeave(_, _ string, _ bool) {}

// TestRoomEventsOnJoinRunsSynchronouslyUnderTheEmitLock confirms OnJoin
// runs inline, at the end of onJoinRoom, still inside the room's emit lock,
// not dispatched with `go`. Alice's own room-user-change emit fires before
// her OnJoin call, so her own round trip cannot show the difference;
// instead, this test proves it the way it actually matters: while Alice's
// slow OnJoin holds room1's emit lock, Bob's own join-room call for the
// same room must wait for that lock before it can even register, so his
// round trip is delayed by (at least) Alice's hook.
func TestRoomEventsOnJoinRunsSynchronouslyUnderTheEmitLock(t *testing.T) {
	const delay = 200 * time.Millisecond
	events := &gatedRoomEvents{slowUserID: "alice", delay: delay, started: make(chan struct{})}
	rel := New(testConfig(), fakeVerifier{
		"alice-token": {FileID: "room1", UserID: "alice", UserName: "Alice", CanWrite: true},
		"bob-token":   {FileID: "room1", UserID: "bob", UserName: "Bob", CanWrite: true},
	}, WithRoomEvents(events))
	defer rel.Close()

	a := newPollingClient(t, rel.Handler())
	a.connect(`{"token":"alice-token"}`)
	a.poll() // CONNECT ack
	a.poll() // init-room

	b := newPollingClient(t, rel.Handler())
	b.connect(`{"token":"bob-token"}`)
	b.poll() // CONNECT ack
	b.poll() // init-room

	a.emit("join-room", "room1")

	select {
	case <-events.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Alice's OnJoin to start")
	}

	start := time.Now()
	b.emit("join-room", "room1")
	b.poll() // room-user-change: proves Bob's own join actually landed

	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("Bob's join-room round trip took %s, want at least %s: it must serialize behind Alice's still-running OnJoin under room1's emit lock", elapsed, delay)
	}
}

// lockingRoomEvents is a RoomEvents whose OnJoin/OnLeave guard their own
// bookkeeping with a lock, the way a real implementation (internal/room's
// Manager) guards its state. Driving many concurrent real joins and leaves
// through it exercises the actual composition of that hook lock with the
// relay's own per-room emit lock: a bad interaction between the two would
// show up here as a deadlock, not merely as a passing assertion.
type lockingRoomEvents struct {
	mu    sync.Mutex
	joins atomic.Int64
	leave atomic.Int64
}

func (e *lockingRoomEvents) OnJoin(_, _ string, _ bool) {
	e.mu.Lock()
	e.joins.Add(1)
	e.mu.Unlock()
}

func (e *lockingRoomEvents) OnLeave(_, _ string, _ bool) {
	e.mu.Lock()
	e.leave.Add(1)
	e.mu.Unlock()
}

// TestRoomEventsDoNotDeadlockUnderConcurrentRealJoinsAndLeaves drives many
// concurrent real connections through rel.Handler(), then joins and leaves
// each one for the same room by calling onJoinRoom/onDisconnecting
// directly against its captured *socket.Socket (the same pattern
// TestOnDisconnectingRunsAfterAPendingJoinRoomTask uses). A deadlock
// between the registry's mutex, the per-room emit lock, and the hook's
// own lock would hang this test forever; the completion channel plus a
// hard timeout turns that into a clean failure instead. The test proves
// no deadlock and full delivery (every OnJoin and OnLeave lands), not a
// particular event order.
func TestRoomEventsDoNotDeadlockUnderConcurrentRealJoinsAndLeaves(t *testing.T) {
	events := &lockingRoomEvents{}

	const n = 50
	verifier := make(fakeVerifier, n)
	tokens := make([]string, n)
	for i := range n {
		tok := "tok-" + strconv.Itoa(i)
		tokens[i] = tok
		verifier[tok] = Session{FileID: "stress-room", UserID: tok, UserName: tok, CanWrite: true}
	}

	rel := New(testConfig(), verifier, WithRoomEvents(events))
	defer rel.Close()

	sockets := make(chan *socket.Socket, n)
	if err := rel.io.On("connection", func(clients ...any) {
		if s, ok := clients[0].(*socket.Socket); ok {
			sockets <- s
		}
	}); err != nil {
		t.Fatalf("register connection listener: %v", err)
	}

	// rel.Handler() must be called once, ahead of the concurrent goroutines
	// below, and its result shared: the underlying library's own handler
	// setup is not itself safe to call concurrently, and every other test
	// in this package already follows that same one-call rule.
	h := rel.Handler()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			c := newPollingClient(t, h)
			c.connect(`{"token":"` + tokens[i] + `"}`)
			c.poll() // CONNECT ack
			c.poll() // init-room

			s := <-sockets
			rel.onJoinRoom(s)("stress-room")
			rel.onDisconnecting(s)()
		}(i)
	}
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out: concurrent real joins/leaves through the handler did not complete, possible deadlock")
	}

	deadline := time.After(2 * time.Second)
	for events.joins.Load() < n || events.leave.Load() < n {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for OnJoin/OnLeave to finish: joins=%d leaves=%d, want %d each",
				events.joins.Load(), events.leave.Load(), n)
		case <-time.After(time.Millisecond):
		}
	}
}
