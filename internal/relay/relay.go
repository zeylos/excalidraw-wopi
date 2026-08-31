// Package relay runs the socket.io realtime layer: server setup, handshake
// auth, rooms, and presence, plus broadcasts, the volatile channel, image
// relay, and syncer election, built on top of the registry in rooms.go.
package relay

import (
	"log/slog"
	"net/http"
	"regexp"

	socket "github.com/zishang520/socket.io/servers/socket/v3"
	"github.com/zishang520/socket.io/v3/pkg/types"

	"github.com/zeylos/excalidraw-wopi/internal/config"
)

// authErrorMessage is the exact string the frontend's connect_error
// circuit breaker matches on. Do not change it without updating the
// frontend in lockstep.
const authErrorMessage = "Authentication error"

// wrongReplicaMessage rejects a handshake whose session names a file this
// replica does not own (multi-replica routing, internal/peers). The
// string must stay exactly this value: web/src/hooks/useCollaboration.ts
// matches it and rebuilds the socket itself, so a fresh handshake reaches
// internal/peers' Middleware and routes to the current owner. Do not
// reuse authErrorMessage here: the client treats that string as terminal
// and stops reconnecting.
const wrongReplicaMessage = "wrong replica"

var roomIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

const (
	// maxVolatileBytes caps a server-volatile-broadcast payload (cursor
	// and viewport updates are tiny wire messages), well under the 60 MB
	// socket buffer a client could otherwise fill before this handler
	// gets to look at it.
	maxVolatileBytes = 64 * 1024
	// maxImageIDBytes caps an image-get request's imageID argument.
	maxImageIDBytes = 256
)

// Session is the identity and permission set a verified session JWT
// carries into the relay. internal/session builds the concrete verifier;
// the orchestrator adapts it to TokenVerifier, so this package stays free
// of that import.
type Session struct {
	FileID   string
	UserID   string
	UserName string
	CanWrite bool
}

// TokenVerifier checks a session JWT from a socket.io handshake and
// returns the identity it carries.
type TokenVerifier interface {
	Verify(raw string) (Session, error)
}

// RoomEvents lets a caller track live room membership across every socket
// join and leave, so it can drive per-room lifecycle decisions (lock
// acquisition, the final flush, and unlock) that belong outside this
// package. The relay calls OnJoin/OnLeave synchronously, on the leaving or
// joining socket's own task-queue goroutine, while that room's emit lock
// (see roomEmitLocks) is held: a `go` dispatch here would let a leave
// overtake an earlier join in delivery order. An implementation
// must therefore return promptly: it must not block, must not do I/O, and
// must never call back into the Relay, or it stalls that socket's queue
// and every other join/leave the emit lock is serializing for the same
// room.
type RoomEvents interface {
	// OnJoin fires once a socket newly joins fileID's room (a repeat
	// join from an already-present socket does not fire it again).
	OnJoin(fileID, userID string, canWrite bool)
	// OnLeave fires once a socket leaves fileID's room. roomEmpty
	// reports whether that departure left the room with no member at
	// all.
	OnLeave(fileID, userID string, roomEmpty bool)
}

// Option configures optional Relay behavior.
type Option func(*Relay)

// WithRoomEvents sets the RoomEvents hook the relay calls at every
// registry join and leave.
func WithRoomEvents(events RoomEvents) Option {
	return func(r *Relay) { r.events = events }
}

// WithOwnershipCheck sets the function authenticate uses to reject a
// handshake for a file this replica does not own (multi-replica routing,
// internal/peers). A nil check, the default, treats every replica as the
// owner of every file.
func WithOwnershipCheck(isOwner func(fileID string) bool) Option {
	return func(r *Relay) { r.isOwner = isOwner }
}

// Relay runs the socket.io server: handshake auth, one room per WOPI file,
// presence, broadcasts, and syncer election. Build one with New.
type Relay struct {
	io            *socket.Server
	rooms         *registry
	verifier      TokenVerifier
	maxSceneBytes int64
	events        RoomEvents
	emitLocks     *roomEmitLocks
	isOwner       func(fileID string) bool
}

// New builds a Relay. It does not start listening; mount Handler() on a
// mux to serve it.
func New(cfg *config.Config, verifier TokenVerifier, opts ...Option) *Relay {
	socketOpts := socket.DefaultServerOptions()
	socketOpts.SetMaxHttpBufferSize(cfg.SocketBufferBytes)
	socketOpts.SetTransports(types.NewSet(socket.Polling, socket.WebSocket))
	socketOpts.SetAllowEIO3(false)

	rel := &Relay{
		io:            socket.NewServer(nil, socketOpts),
		rooms:         newRegistry(),
		verifier:      verifier,
		maxSceneBytes: cfg.MaxSceneBytes,
		emitLocks:     newRoomEmitLocks(),
	}
	for _, opt := range opts {
		opt(rel)
	}

	rel.io.Use(rel.authenticate)
	// On never returns a non-nil error for a non-nil listener.
	_ = rel.io.On("connection", rel.onConnection)

	return rel
}

// Handler returns the socket.io HTTP handler. Mount it at /socket.io/.
func (rel *Relay) Handler() http.Handler {
	return rel.io.ServeHandler(nil)
}

// Close shuts down the server and every connected socket.
func (rel *Relay) Close() {
	rel.io.Close(nil)
}

// BroadcastToRoom sends event with payload to every socket joined to
// fileID's room, marshaled as JSON on the wire like any other socket.io
// emit. Unlike the server-broadcast/server-volatile-broadcast channels,
// this push has no client behind it: a server-side caller (the conflict-state
// notification in internal/room) drives it directly, from whatever goroutine
// detects the state change, not from inside a socket handler.
//
// It is safe to call this way: rel.io.To(room).Emit ultimately reads and
// writes the adapter's room membership through *types.Map (a sync.Map-backed
// collection, see zishang520/socket.io/servers/socket/v3's adapter.go), the
// same structure every socket handler's own To(room).Emit call already goes
// through concurrently with every other connected socket's handler
// goroutine. This method adds no state of its own, so it needs no extra
// lock; TestBroadcastToRoomReachesAJoinedMember exercises the call from a
// plain goroutine, not a handler, to pin that down.
func (rel *Relay) BroadcastToRoom(fileID, event string, payload any) {
	room := socket.Room(fileID)
	if err := rel.io.To(room).Emit(event, payload); err != nil {
		slog.Warn("relay: broadcast emit failed", "room", fileID, "event", event, "error", err)
	}
}

// authenticate reads the handshake's auth token, verifies it, checks that
// this replica owns the session's file, and stores the resulting Session
// on the socket. A verify failure must use exactly authErrorMessage: the
// frontend circuit breaker matches on it. A wrong-replica rejection uses
// wrongReplicaMessage instead, so the client keeps retrying.
func (rel *Relay) authenticate(s *socket.Socket, next func(*socket.ExtendedError)) {
	token, _ := s.Handshake().Auth["token"].(string)

	sess, err := rel.verifier.Verify(token)
	if err != nil {
		next(socket.NewExtendedError(authErrorMessage, nil))
		return
	}
	if rel.isOwner != nil && !rel.isOwner(sess.FileID) {
		next(socket.NewExtendedError(wrongReplicaMessage, nil))
		return
	}

	s.SetData(&socketState{Session: sess})
	next(nil)
}

// socketState is a socket's mutable relay state. A socket's handlers run
// serially on that socket's own queue goroutine, so no extra lock guards
// this beyond the registry's own mutex.
type socketState struct {
	Session
	roomID string
}

func (rel *Relay) onConnection(clients ...any) {
	s, ok := clients[0].(*socket.Socket)
	if !ok {
		return
	}

	if err := s.Emit("init-room"); err != nil {
		slog.Warn("relay: init-room emit failed", "socket", s.Id(), "error", err)
	}

	// On never returns a non-nil error for a non-nil listener.
	_ = s.On("join-room", rel.onJoinRoom(s))
	_ = s.On("server-broadcast", rel.onServerBroadcast(s))
	_ = s.On("server-volatile-broadcast", rel.onServerVolatileBroadcast(s))
	_ = s.On("image-get", rel.onImageGet(s))
	_ = s.On("disconnecting", rel.onDisconnecting(s))
}

// onJoinRoom handles join-room. It validates the room id, checks it
// against the session's fileId claim, and is idempotent for a socket that
// already joined: a repeat call joins neither the registry nor the
// socket.io room a second time, and it sends no repeat presence emit.
func (rel *Relay) onJoinRoom(s *socket.Socket) func(...any) {
	return func(args ...any) {
		roomID, state, ok := roomGuard(s, args, 1, true)
		if !ok {
			return
		}

		member := Member{
			SocketID: string(s.Id()),
			UserID:   state.UserID,
			UserName: state.UserName,
			CanWrite: state.CanWrite,
		}

		// Holds roomID's emit lock across the registry mutation and every
		// emit it drives, so a concurrent join or leave for this room
		// cannot interleave its own mutate-then-emit sequence with this
		// one (see roomEmitLocks's doc comment).
		lock := rel.emitLocks.lock(roomID)
		defer lock.Unlock()

		// join-room runs on this socket's own queue, so it can sit behind
		// other queued work; by the time it is this call's turn, the
		// socket may already have disconnected. Re-checking here, right
		// before the registry insertion, closes the residual window where
		// a since-closed socket would otherwise become a permanent ghost
		// member: disconnecting already ran (and found nothing to remove,
		// since roomID was not yet set) before this queued join ever adds
		// one.
		if !s.Connected() {
			return
		}

		presenceList, added, isSyncer := rel.rooms.join(roomID, member)
		if !added {
			return
		}
		state.roomID = roomID

		room := socket.Room(roomID)
		s.Join(room)

		if err := rel.io.To(room).Emit("room-user-change", wirePresence(presenceList)); err != nil {
			slog.Warn("relay: room-user-change emit failed", "room", roomID, "error", err)
		}
		if err := rel.io.To(room).Emit("user-joined", userJoinedPayload{
			UserID:   state.UserID,
			UserName: state.UserName,
			SocketID: string(s.Id()),
			IsSyncer: isSyncer,
		}); err != nil {
			slog.Warn("relay: user-joined emit failed", "room", roomID, "error", err)
		}
		// The frontend waits for sync-designate before it starts saving; every
		// joiner gets one, even a false one.
		if err := s.Emit("sync-designate", syncDesignatePayload{IsSyncer: isSyncer}); err != nil {
			slog.Warn("relay: sync-designate emit failed", "socket", s.Id(), "error", err)
		}

		if rel.events != nil {
			// Called synchronously, still inside roomID's emit lock: a
			// `go` dispatch would leave join/leave delivery order to
			// goroutine scheduling, so a fast refresh (leave then
			// rejoin) could tell the Manager about the join before the
			// leave, causing a premature unlock and, with liveUserCount
			// left permanently positive, a room that never closes. Manager
			// itself never calls back into the relay from OnJoin, so this
			// cannot deadlock against the emit lock held here.
			rel.events.OnJoin(roomID, state.UserID, state.CanWrite)
		}
	}
}

// onDisconnecting handles disconnecting. Its membership in the socket.io
// adapter's own room may already be gone by the time this runs: the body
// executes via s.Enqueue (see below), and the library's own cleanup can
// run ahead of that queued call on the transport goroutine. The handler
// deliberately never reads adapter membership; it removes the socket from
// this package's own registry instead, tells the remaining members the
// leaver is gone, and pushes any sync-designate change the departure
// causes.
//
// The library (zishang520 v3.0.4) emits "disconnecting" synchronously on
// the transport goroutine, in Socket._onclose, strictly before it calls
// TryClose on this socket's own task queue; every other event (including
// join-room) instead runs through that queue, via Socket.Enqueue. Left
// alone, that means disconnecting can run concurrently with, or even
// before, a join-room call still sitting in the queue: the join-room
// handler's write to socketState races this handler's read of it, and if
// join-room has not run yet this handler sees roomID == "" and does
// nothing, yet the queued join-room call still runs afterward (TryClose
// only stops new enqueues; it drains what is already queued) and adds a
// member the registry then has no way to ever remove — a permanent ghost
// that, if it holds the syncer role, strands the room. Wrapping this
// handler's body in s.Enqueue serializes it behind any join-room call
// already queued for this socket, so by the time it runs, state.roomID
// (if the join landed at all) is always set.
func (rel *Relay) onDisconnecting(s *socket.Socket) func(...any) {
	return func(...any) {
		s.Enqueue(func() {
			state, ok := s.Data().(*socketState)
			if !ok || state.roomID == "" {
				return
			}

			// See onJoinRoom's matching lock: this keeps the leave's
			// mutate-then-emit sequence from interleaving with a
			// concurrent join or leave for the same room.
			lock := rel.emitLocks.lock(state.roomID)
			defer lock.Unlock()

			presenceList, remaining, outcome := rel.rooms.leave(state.roomID, string(s.Id()))

			if rel.events != nil {
				// See onJoinRoom's matching call: synchronous, still inside
				// the emit lock, so join/leave delivery order matches
				// registry mutation order.
				rel.events.OnLeave(state.roomID, state.UserID, !remaining)
			}

			if !remaining {
				// No separate map cleanup: the deferred lock.Unlock()
				// above drops the map entry itself once no other caller
				// holds or awaits it.
				return
			}

			room := socket.Room(state.roomID)
			if err := s.Broadcast().To(room).Emit("room-user-change", wirePresence(presenceList)); err != nil {
				slog.Warn("relay: room-user-change emit failed", "room", state.roomID, "error", err)
			}

			if outcome.Changed {
				rel.emitSyncDesignate(outcome.PromotedIDs, true)
				rel.emitSyncDesignate(outcome.DemotedIDs, false)
			}
		})
	}
}

// emitSyncDesignate pushes a sync-designate emit to every socket in
// socketIDs. Each socket auto-joins a room named after its own id, so
// targeting those rooms reaches exactly the named sockets, wherever their
// connections landed.
func (rel *Relay) emitSyncDesignate(socketIDs []string, isSyncer bool) {
	if len(socketIDs) == 0 {
		return
	}
	rooms := make([]socket.Room, len(socketIDs))
	for i, id := range socketIDs {
		rooms[i] = socket.Room(id)
	}
	if err := rel.io.To(rooms...).Emit("sync-designate", syncDesignatePayload{IsSyncer: isSyncer}); err != nil {
		slog.Warn("relay: sync-designate emit failed", "sockets", socketIDs, "isSyncer", isSyncer, "error", err)
	}
}

func roomIDArg(args []any) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	roomID, ok := args[0].(string)
	if !ok || !roomIDPattern.MatchString(roomID) {
		return "", false
	}
	return roomID, true
}

// roomGuard extracts args[0] as a room id, requires at least minArgs
// arguments, and loads this socket's state. The broadcast and image-get
// handlers (forJoin false) compare roomID against state.roomID, the room
// this socket already joined, and fail silently. join-room (forJoin
// true) has not joined a room yet, so it compares against the session's
// FileID claim instead, and emits an "error" event on a malformed room
// id or a claim mismatch rather than failing silently.
func roomGuard(s *socket.Socket, args []any, minArgs int, forJoin bool) (roomID string, state *socketState, ok bool) {
	roomID, ok = roomIDArg(args)
	if len(args) < minArgs || !ok {
		if forJoin {
			if err := s.Emit("error", "join-room: invalid room id"); err != nil {
				slog.Warn("relay: error emit failed", "socket", s.Id(), "error", err)
			}
		}
		return "", nil, false
	}

	state, ok = s.Data().(*socketState)
	if !ok {
		return "", nil, false
	}

	want := state.roomID
	if forJoin {
		want = state.FileID
	}
	if want != roomID {
		if forJoin {
			if err := s.Emit("error", "join-room: room does not match the session"); err != nil {
				slog.Warn("relay: error emit failed", "socket", s.Id(), "error", err)
			}
		}
		return "", nil, false
	}

	return roomID, state, true
}

// presenceUser is the wire shape of a room-user-change entry's user field.
type presenceUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// presenceEntry is the wire shape of one room-user-change row: one per
// distinct user, deduped across that user's sockets.
type presenceEntry struct {
	SocketID  string       `json:"socketId"`
	User      presenceUser `json:"user"`
	UserID    string       `json:"userId"`
	SocketIDs []string     `json:"socketIds"`
}

func wirePresence(list []UserPresence) []presenceEntry {
	out := make([]presenceEntry, len(list))
	for i, u := range list {
		out[i] = presenceEntry{
			SocketID:  u.SocketID,
			User:      presenceUser{ID: u.UserID, Name: u.UserName},
			UserID:    u.UserID,
			SocketIDs: u.SocketIDs,
		}
	}
	return out
}

// userJoinedPayload is the wire shape of the user-joined event. IsSyncer
// carries the joiner's syncer status after election (rooms.go).
type userJoinedPayload struct {
	UserID   string `json:"userId"`
	UserName string `json:"userName"`
	SocketID string `json:"socketId"`
	IsSyncer bool   `json:"isSyncer"`
}

// syncDesignatePayload is the wire shape of the sync-designate event.
type syncDesignatePayload struct {
	IsSyncer bool `json:"isSyncer"`
}

// onServerBroadcast handles server-broadcast: the scene sync channel. The
// relay must not parse this payload, so beyond the identity-bearing
// type-tag guard below, it relays the bytes exactly as received, including
// the vestigial iv argument.
func (rel *Relay) onServerBroadcast(s *socket.Socket) func(...any) {
	return func(args ...any) {
		roomID, state, ok := roomGuard(s, args, 2, false)
		if !ok {
			return
		}
		if !state.CanWrite {
			slog.Debug("relay: server-broadcast dropped: read-only session", "socket", s.Id(), "room", roomID)
			return
		}
		payload, ok := bytesArg(args[1])
		if !ok {
			return
		}
		// MOUSE_LOCATION and VIEWPORT_UPDATE carry a client-asserted
		// identity field the relay only ever rewrites on the volatile
		// channel (rewriteVolatile, onServerVolatileBroadcast). Left
		// unchecked, a writer could send one of those two types here
		// instead and spoof another user's cursor identity. This is the
		// one type tag this handler ever inspects; every other type
		// still relays as an opaque blob.
		if isIdentityBearingType(payload) {
			slog.Debug("relay: server-broadcast dropped: identity-bearing type must use the volatile channel", "socket", s.Id(), "room", roomID)
			if err := s.Emit("error", "server-broadcast: this type must use the volatile channel"); err != nil {
				slog.Warn("relay: error emit failed", "socket", s.Id(), "error", err)
			}
			return
		}
		// MaxImageBytes is not enforced here: an IMAGE_ADD payload sits
		// inside this opaque blob, so checking it would mean parsing
		// every server-broadcast, which this relay must not do. It is
		// enforced client-side (web/src/utils/imageSizeLimit.ts) and
		// again at the save endpoint (boardapi's MaxSceneBytes check);
		// the scene-wide cap below is the transport backstop.
		if exceedsSceneLimit(payload, rel.maxSceneBytes) {
			slog.Debug("relay: server-broadcast dropped: payload too large", "socket", s.Id(), "room", roomID, "bytes", len(payload))
			if err := s.Emit("error", "server-broadcast: payload exceeds the size limit"); err != nil {
				slog.Warn("relay: error emit failed", "socket", s.Id(), "error", err)
			}
			return
		}

		var iv any = []any{}
		if len(args) > 2 {
			iv = args[2]
		}

		room := socket.Room(roomID)
		if err := s.Broadcast().To(room).Emit("client-broadcast", payload, iv); err != nil {
			slog.Warn("relay: client-broadcast emit failed", "room", roomID, "error", err)
		}
	}
}

// onServerVolatileBroadcast handles server-volatile-broadcast: cursor and
// viewport updates. Unlike server-broadcast, read-only sessions may send
// on this channel: cursors stay visible in view mode. The payload is
// parsed to stamp the server-side identity.
func (rel *Relay) onServerVolatileBroadcast(s *socket.Socket) func(...any) {
	return func(args ...any) {
		roomID, state, ok := roomGuard(s, args, 2, false)
		if !ok {
			return
		}
		raw, ok := bytesArg(args[1])
		if !ok {
			return
		}
		if len(raw) > maxVolatileBytes {
			slog.Debug("relay: server-volatile-broadcast dropped: payload too large", "socket", s.Id(), "room", roomID, "bytes", len(raw))
			return
		}
		out, ok := rewriteVolatile(raw, state.Session)
		if !ok {
			slog.Debug("relay: server-volatile-broadcast dropped: unparseable or unknown type", "socket", s.Id(), "room", roomID)
			return
		}

		room := socket.Room(roomID)
		if err := s.Volatile().Broadcast().To(room).Emit("client-broadcast", out); err != nil {
			slog.Warn("relay: client-broadcast (volatile) emit failed", "room", roomID, "error", err)
		}
	}
}

// onImageGet handles image-get: a client asks the room for an image it is
// missing. The relay holds no image store; it just asks the rest of the
// room to answer.
func (rel *Relay) onImageGet(s *socket.Socket) func(...any) {
	return func(args ...any) {
		roomID, _, ok := roomGuard(s, args, 2, false)
		if !ok {
			return
		}
		imageID, ok := args[1].(string)
		if !ok {
			return
		}
		if len(imageID) > maxImageIDBytes {
			slog.Debug("relay: image-get dropped: imageID too large", "socket", s.Id(), "bytes", len(imageID))
			return
		}

		payload, err := imageRequestBytes(imageID)
		if err != nil {
			slog.Warn("relay: encode image-get request failed", "socket", s.Id(), "error", err)
			return
		}

		room := socket.Room(roomID)
		if err := s.Broadcast().To(room).Emit("client-broadcast", payload); err != nil {
			slog.Warn("relay: client-broadcast (image-get) emit failed", "room", roomID, "error", err)
		}
	}
}

// bytesArg reads arg as a binary socket.io attachment. Inbound binary
// arrives as types.BufferInterface; a plain []byte is accepted too, for
// callers (and tests) that build args directly.
func bytesArg(arg any) ([]byte, bool) {
	switch v := arg.(type) {
	case []byte:
		return v, true
	case types.BufferInterface:
		return v.Bytes(), true
	default:
		return nil, false
	}
}
