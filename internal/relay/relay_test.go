package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	socket "github.com/zishang520/socket.io/servers/socket/v3"

	"github.com/zeylos/excalidraw-wopi/internal/config"
)

// fakeVerifier is a TokenVerifier stub for tests: it maps a fixed set of
// raw tokens to a Session, and rejects anything else.
type fakeVerifier map[string]Session

func (f fakeVerifier) Verify(raw string) (Session, error) {
	sess, ok := f[raw]
	if !ok {
		return Session{}, errors.New("unknown token")
	}
	return sess, nil
}

func testConfig() *config.Config {
	return &config.Config{SocketBufferBytes: 8 * 1024 * 1024}
}

// pollingClient drives the Engine.IO v4 long-polling wire protocol
// directly against a Relay's http.Handler with httptest.NewRecorder, one
// packet at a time. It calls ServeHTTP in-process, without a live network
// connection: this repo's sandbox refuses loopback TCP outright (verified
// with a bare net.Dial, independent of this package), so an
// httptest.Server plus a real socket.io-client cannot run here. This is a
// minimal socket-level assertion via an HTTP handshake request; the Node
// smoke test is the real socket.io-client v4 interop signal.
type pollingClient struct {
	t   *testing.T
	h   http.Handler
	sid string
	// pending holds engine.io packets a prior long-poll GET returned but
	// poll() has not yet handed to a caller: the server can batch several
	// packets (record-separator '\x1e' joined) into one response when a
	// handler emits more than once before the next GET arrives.
	pending [][]byte
}

func newPollingClient(t *testing.T, h http.Handler) *pollingClient {
	t.Helper()
	c := &pollingClient{t: t, h: h}

	body := c.nextPacket()
	openBody := c.mustPacket(body, '0')

	var open struct {
		Sid string `json:"sid"`
	}
	if err := json.Unmarshal(openBody, &open); err != nil {
		t.Fatalf("decode engine.io open packet %q: %v", openBody, err)
	}
	c.sid = open.Sid
	return c
}

func (c *pollingClient) do(method, body string, header http.Header) *httptest.ResponseRecorder {
	c.t.Helper()
	url := "/socket.io/?EIO=4&transport=polling"
	if c.sid != "" {
		url += "&sid=" + c.sid
	}

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, url, reader)
	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

// connect POSTs a socket.io CONNECT packet with authPayload as the auth
// object (raw JSON, e.g. `{"token":"..."}`).
func (c *pollingClient) connect(authPayload string) {
	c.t.Helper()
	rec := c.do(http.MethodPost, "40"+authPayload, http.Header{"Content-Type": {"text/plain;charset=UTF-8"}})
	if rec.Code != http.StatusOK {
		c.t.Fatalf("POST connect: status %d", rec.Code)
	}
}

// emit POSTs a socket.io EVENT packet: ev plus args, JSON-encoded as a
// socket.io-style array.
func (c *pollingClient) emit(ev string, args ...any) {
	c.t.Helper()
	packet := append([]any{ev}, args...)
	raw, err := json.Marshal(packet)
	if err != nil {
		c.t.Fatalf("encode emit(%s): %v", ev, err)
	}
	rec := c.do(http.MethodPost, "42"+string(raw), http.Header{"Content-Type": {"text/plain;charset=UTF-8"}})
	if rec.Code != http.StatusOK {
		c.t.Fatalf("POST emit(%s): status %d", ev, rec.Code)
	}
}

// poll returns the next socket.io packet's type byte plus its raw body,
// pulling a fresh long-poll GET only when the pending queue is empty.
func (c *pollingClient) poll() (byte, []byte) {
	c.t.Helper()
	return c.mustSocketIOPacket(c.nextPacket())
}

// nextPacket returns the next queued engine.io packet, blocking on a new
// long-poll GET (and splitting a batched response on '\x1e') when the
// queue is empty.
func (c *pollingClient) nextPacket() []byte {
	c.t.Helper()
	if len(c.pending) == 0 {
		rec := c.do(http.MethodGet, "", nil)
		body, err := io.ReadAll(rec.Result().Body)
		if err != nil {
			c.t.Fatalf("read response body: %v", err)
		}
		if rec.Code != http.StatusOK || len(body) == 0 {
			c.t.Fatalf("engine.io poll: status %d body %q, want status 200 with a body", rec.Code, body)
		}
		c.pending = append(c.pending, bytes.Split(body, []byte{0x1e})...)
	}

	packet := c.pending[0]
	c.pending = c.pending[1:]
	return packet
}

// mustPacket asserts one engine.io packet of type wantType and returns
// its body (the bytes after the type character).
func (c *pollingClient) mustPacket(body []byte, wantType byte) []byte {
	c.t.Helper()
	if len(body) == 0 || body[0] != wantType {
		c.t.Fatalf("engine.io packet = %q, want type %q", body, wantType)
	}
	return body[1:]
}

// mustSocketIOPacket asserts an engine.io MESSAGE packet ('4') carrying a
// socket.io packet, and returns the socket.io packet's type byte and body.
func (c *pollingClient) mustSocketIOPacket(body []byte) (byte, []byte) {
	c.t.Helper()
	msg := c.mustPacket(body, '4')
	if len(msg) == 0 {
		c.t.Fatal("empty socket.io packet")
	}
	return msg[0], msg[1:]
}

// TestRelayRejectsBadTokenWithExactErrorString drives the wire handshake
// with an unknown token and checks the exact CONNECT_ERROR message the
// frontend's circuit breaker matches on.
func TestRelayRejectsBadTokenWithExactErrorString(t *testing.T) {
	rel := New(testConfig(), fakeVerifier{"good-token": {FileID: "room1", UserID: "u1", UserName: "Alice", CanWrite: true}})
	defer rel.Close()

	c := newPollingClient(t, rel.Handler())
	c.connect(`{"token":"bad-token"}`)

	pktType, body := c.poll()
	if pktType != '4' {
		t.Fatalf("socket.io packet type = %q, want '4' (CONNECT_ERROR)", pktType)
	}

	var errPayload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &errPayload); err != nil {
		t.Fatalf("decode CONNECT_ERROR payload %q: %v", body, err)
	}
	if errPayload.Message != authErrorMessage {
		t.Fatalf("CONNECT_ERROR message = %q, want %q", errPayload.Message, authErrorMessage)
	}
}

// TestRelayRejectsRefusedFileIDWithExactErrorString checks the
// multi-replica invariant: a valid token whose fileID WithOwnershipCheck
// refuses gets rejected with exactly wrongReplicaMessage, not
// authErrorMessage, since the client only keeps retrying on the former.
func TestRelayRejectsRefusedFileIDWithExactErrorString(t *testing.T) {
	rel := New(testConfig(), fakeVerifier{"good-token": {FileID: "room1", UserID: "u1", UserName: "Alice", CanWrite: true}},
		WithOwnershipCheck(func(_ string) bool { return false }))
	defer rel.Close()

	c := newPollingClient(t, rel.Handler())
	c.connect(`{"token":"good-token"}`)

	pktType, body := c.poll()
	if pktType != '4' {
		t.Fatalf("socket.io packet type = %q, want '4' (CONNECT_ERROR)", pktType)
	}

	var errPayload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &errPayload); err != nil {
		t.Fatalf("decode CONNECT_ERROR payload %q: %v", body, err)
	}
	if errPayload.Message != wrongReplicaMessage {
		t.Fatalf("CONNECT_ERROR message = %q, want %q", errPayload.Message, wrongReplicaMessage)
	}
}

// TestRelayJoinRoomFlow drives connect, init-room, join-room, and
// room-user-change end to end over the wire protocol. e2e/interop covers
// the same flow against a live process with a real socket.io-client v4.
func TestRelayJoinRoomFlow(t *testing.T) {
	rel := New(testConfig(), fakeVerifier{"good-token": {FileID: "room1", UserID: "u1", UserName: "Alice", CanWrite: true}})
	defer rel.Close()

	c := newPollingClient(t, rel.Handler())
	c.connect(`{"token":"good-token"}`)

	if pktType, body := c.poll(); pktType != '0' {
		t.Fatalf("first packet after connect = type %q body %q, want '0' (CONNECT ack)", pktType, body)
	}

	pktType, body := c.poll()
	if pktType != '2' {
		t.Fatalf("packet type = %q, want '2' (EVENT); body=%q", pktType, body)
	}
	var initRoomEvent []string
	if err := json.Unmarshal(body, &initRoomEvent); err != nil || len(initRoomEvent) == 0 || initRoomEvent[0] != "init-room" {
		t.Fatalf("event = %q, want [\"init-room\"]", body)
	}

	c.emit("join-room", "room1")

	pktType, body = c.poll()
	if pktType != '2' {
		t.Fatalf("packet type = %q, want '2' (EVENT); body=%q", pktType, body)
	}
	var roomUserChangeEvent struct {
		Event   string
		Entries []presenceEntry
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) != 2 {
		t.Fatalf("room-user-change event = %q, want a 2-element array", body)
	}
	_ = json.Unmarshal(raw[0], &roomUserChangeEvent.Event)
	_ = json.Unmarshal(raw[1], &roomUserChangeEvent.Entries)
	if roomUserChangeEvent.Event != "room-user-change" {
		t.Fatalf("event name = %q, want room-user-change", roomUserChangeEvent.Event)
	}
	if len(roomUserChangeEvent.Entries) != 1 {
		t.Fatalf("room-user-change entries = %d, want 1: %+v", len(roomUserChangeEvent.Entries), roomUserChangeEvent.Entries)
	}
	if entry := roomUserChangeEvent.Entries[0]; entry.UserID != "u1" || entry.User.Name != "Alice" || len(entry.SocketIDs) != 1 {
		t.Fatalf("room-user-change entry = %+v, want userId u1 / name Alice / one socket id", entry)
	}

	pktType, body = c.poll()
	if pktType != '2' {
		t.Fatalf("packet type = %q, want '2' (EVENT); body=%q", pktType, body)
	}
	var userJoinedRaw []json.RawMessage
	if err := json.Unmarshal(body, &userJoinedRaw); err != nil || len(userJoinedRaw) != 2 {
		t.Fatalf("user-joined event = %q, want a 2-element array", body)
	}
	var userJoinedName string
	var userJoined userJoinedPayload
	_ = json.Unmarshal(userJoinedRaw[0], &userJoinedName)
	_ = json.Unmarshal(userJoinedRaw[1], &userJoined)
	if userJoinedName != "user-joined" {
		t.Fatalf("event name = %q, want user-joined", userJoinedName)
	}
	// Alice is the room's only member and can write, so she wins the empty
	// syncer slot.
	if userJoined.UserID != "u1" || userJoined.UserName != "Alice" || !userJoined.IsSyncer {
		t.Fatalf("user-joined payload = %+v, want userId u1 / userName Alice / isSyncer true", userJoined)
	}

	pktType, body = c.poll()
	if pktType != '2' {
		t.Fatalf("packet type = %q, want '2' (EVENT); body=%q", pktType, body)
	}
	var syncDesignateRaw []json.RawMessage
	if err := json.Unmarshal(body, &syncDesignateRaw); err != nil || len(syncDesignateRaw) != 2 {
		t.Fatalf("sync-designate event = %q, want a 2-element array", body)
	}
	var syncDesignateName string
	var syncDesignate syncDesignatePayload
	_ = json.Unmarshal(syncDesignateRaw[0], &syncDesignateName)
	_ = json.Unmarshal(syncDesignateRaw[1], &syncDesignate)
	if syncDesignateName != "sync-designate" {
		t.Fatalf("event name = %q, want sync-designate", syncDesignateName)
	}
	if !syncDesignate.IsSyncer {
		t.Fatalf("sync-designate payload = %+v, want isSyncer true", syncDesignate)
	}
}

// TestOnDisconnectingRunsAfterAPendingJoinRoomTask is the regression test
// for the fact that the socket.io library emits "disconnecting"
// synchronously on the transport goroutine, ahead of this socket's own
// task queue (see onDisconnecting's doc comment). Without wrapping its
// body in s.Enqueue, a join-room task already queued when the socket dies
// would still run afterward, adding a ghost member the registry then has
// no way to ever remove.
//
// The wire-level pollingClient cannot express the transport-goroutine
// race itself (every request it drives runs to completion, in-process, one
// at a time, so there is no way to land a real disconnect mid-flight
// against a queued join): this test instead drives onJoinRoom and
// onDisconnecting directly against a captured, real *socket.Socket, in
// exactly the order _onclose produces in production, and checks the queue
// enforces the right outcome regardless of timing.
func TestOnDisconnectingRunsAfterAPendingJoinRoomTask(t *testing.T) {
	rel := New(testConfig(), fakeVerifier{"good-token": {FileID: "room1", UserID: "u1", UserName: "Alice", CanWrite: true}})
	defer rel.Close()

	var captured atomic.Pointer[socket.Socket]
	if err := rel.io.On("connection", func(clients ...any) {
		if s, ok := clients[0].(*socket.Socket); ok {
			captured.Store(s)
		}
	}); err != nil {
		t.Fatalf("register connection listener: %v", err)
	}

	c := newPollingClient(t, rel.Handler())
	c.connect(`{"token":"good-token"}`)
	c.poll() // CONNECT ack
	c.poll() // init-room

	s := captured.Load()
	if s == nil {
		t.Fatal("connection handler never fired: no socket captured")
	}

	// Simulate a join-room packet already queued but not yet run, then the
	// transport tearing the socket down: the same ordering _onclose
	// produces in production.
	s.Enqueue(func() { rel.onJoinRoom(s)("room1") })
	rel.onDisconnecting(s)()

	deadline := time.After(2 * time.Second)
	for {
		members := rel.rooms.members("room1")
		if len(members) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("room1 members = %+v, want none: the queued join ran, so the enqueued disconnect must have cleaned it up", members)
		case <-time.After(time.Millisecond):
		}
	}
}

// TestOnServerBroadcastRejectsIdentityBearingType is the regression test
// checking that a MOUSE_LOCATION or VIEWPORT_UPDATE payload must never
// reach the durable server-broadcast channel, since only
// onServerVolatileBroadcast rewrites the client-asserted identity field. A
// writer sending one there instead must get an error, not have it relayed.
//
// bytesArg only accepts a real []byte or types.BufferInterface argument,
// which the wire-level pollingClient cannot produce (its emit() JSON-encodes
// args, turning a []byte into a base64 string), so this drives the handler
// directly against a captured, real *socket.Socket, the same technique
// TestOnDisconnectingRunsAfterAPendingJoinRoomTask uses.
func TestOnServerBroadcastRejectsIdentityBearingType(t *testing.T) {
	rel := New(testConfig(), fakeVerifier{"good-token": {FileID: "room1", UserID: "u1", UserName: "Alice", CanWrite: true}})
	defer rel.Close()

	var captured atomic.Pointer[socket.Socket]
	if err := rel.io.On("connection", func(clients ...any) {
		if s, ok := clients[0].(*socket.Socket); ok {
			captured.Store(s)
		}
	}); err != nil {
		t.Fatalf("register connection listener: %v", err)
	}

	c := newPollingClient(t, rel.Handler())
	c.connect(`{"token":"good-token"}`)
	c.poll() // CONNECT ack
	c.poll() // init-room

	s := captured.Load()
	if s == nil {
		t.Fatal("connection handler never fired: no socket captured")
	}

	c.emit("join-room", "room1")
	c.poll() // room-user-change
	c.poll() // user-joined
	c.poll() // sync-designate

	payload := []byte(`{"type":"MOUSE_LOCATION","payload":{"user":{"id":"attacker","name":"Evil"}}}`)
	rel.onServerBroadcast(s)("room1", payload, []any{})

	pktType, body := c.poll()
	if pktType != '2' {
		t.Fatalf("packet type = %q, want '2' (EVENT); body=%q", pktType, body)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) != 2 {
		t.Fatalf("event = %q, want a 2-element array", body)
	}
	var eventName, message string
	_ = json.Unmarshal(raw[0], &eventName)
	_ = json.Unmarshal(raw[1], &message)
	if eventName != "error" {
		t.Fatalf("event name = %q, want error: a MOUSE_LOCATION payload must never be relayed on server-broadcast", eventName)
	}
	if !strings.Contains(message, "volatile channel") {
		t.Fatalf("error message = %q, want a mention of the volatile channel", message)
	}
}

// TestOnServerBroadcastStillForwardsSceneTypes checks the identity-bearing
// type guard stays narrow: an opaque scene-type payload (SCENE_UPDATE here,
// standing in for SCENE_INIT/IMAGE_ADD too) must still reach every other
// joined member, unchanged. A []byte argument travels
// the wire as a socket.io binary event: the EVENT packet itself carries a
// placeholder, and the actual bytes follow as a separate base64 ('b')
// engine.io packet, so this reads both.
func TestOnServerBroadcastStillForwardsSceneTypes(t *testing.T) {
	cfg := testConfig()
	cfg.MaxSceneBytes = 1024 // testConfig leaves this 0, which would reject every payload.
	rel := New(cfg, fakeVerifier{
		"alice-token": {FileID: "room1", UserID: "u1", UserName: "Alice", CanWrite: true},
		"bob-token":   {FileID: "room1", UserID: "u2", UserName: "Bob", CanWrite: true},
	})
	defer rel.Close()

	var captured atomic.Pointer[socket.Socket]
	if err := rel.io.On("connection", func(clients ...any) {
		if s, ok := clients[0].(*socket.Socket); ok {
			captured.Store(s)
		}
	}); err != nil {
		t.Fatalf("register connection listener: %v", err)
	}

	alice := newPollingClient(t, rel.Handler())
	alice.connect(`{"token":"alice-token"}`)
	alice.poll() // CONNECT ack
	alice.poll() // init-room

	aliceSocket := captured.Load()
	if aliceSocket == nil {
		t.Fatal("connection handler never fired for Alice: no socket captured")
	}

	alice.emit("join-room", "room1")
	alice.poll() // room-user-change
	alice.poll() // user-joined
	alice.poll() // sync-designate

	bob := newPollingClient(t, rel.Handler())
	bob.connect(`{"token":"bob-token"}`)
	bob.poll() // CONNECT ack
	bob.poll() // init-room
	bob.emit("join-room", "room1")
	bob.poll() // room-user-change
	bob.poll() // user-joined
	bob.poll() // sync-designate
	// Alice's own view of the room also updates once Bob joins.
	alice.poll() // room-user-change
	alice.poll() // user-joined

	payload := []byte(`{"type":"SCENE_UPDATE","payload":{"elements":[]}}`)
	rel.onServerBroadcast(aliceSocket)("room1", payload, []any{})

	pktType, body := bob.poll()
	if pktType != '5' {
		t.Fatalf("packet type = %q, want '5' (BINARY_EVENT); body=%q", pktType, body)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(bytes.TrimLeft(body, "0123456789-"), &raw); err != nil || len(raw) < 2 {
		t.Fatalf("event = %q, want at least a 2-element array (event name, payload placeholder)", body)
	}
	var eventName string
	_ = json.Unmarshal(raw[0], &eventName)
	if eventName != "client-broadcast" {
		t.Fatalf("event name = %q, want client-broadcast: an opaque scene type must still relay", eventName)
	}

	// The attachment is its own engine.io packet, type 'b' (base64
	// binary), not wrapped in a socket.io MESSAGE ('4') packet the way
	// every other event here is; poll() enforces '4', so this reads the
	// raw engine.io packet directly instead.
	attPacket := bob.nextPacket()
	if len(attPacket) == 0 || attPacket[0] != 'b' {
		t.Fatalf("attachment packet = %q, want type 'b' (base64 binary)", attPacket)
	}
	gotPayload, err := base64.StdEncoding.DecodeString(string(attPacket[1:]))
	if err != nil {
		t.Fatalf("decode base64 attachment %q: %v", attPacket[1:], err)
	}
	if string(gotPayload) != string(payload) {
		t.Fatalf("payload = %q, want %q unchanged: the relay must not alter payload bytes", gotPayload, payload)
	}
}
