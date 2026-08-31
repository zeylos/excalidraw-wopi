// Package app wires the service's components into one HTTP server. It
// exists so cmd/excalidraw-wopi/main.go stays a thin process shell.
package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/boardapi"
	"github.com/zeylos/excalidraw-wopi/internal/config"
	"github.com/zeylos/excalidraw-wopi/internal/discovery"
	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/httpserver"
	"github.com/zeylos/excalidraw-wopi/internal/launch"
	"github.com/zeylos/excalidraw-wopi/internal/peers"
	"github.com/zeylos/excalidraw-wopi/internal/proof"
	"github.com/zeylos/excalidraw-wopi/internal/relay"
	"github.com/zeylos/excalidraw-wopi/internal/room"
	"github.com/zeylos/excalidraw-wopi/internal/session"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
	"github.com/zeylos/excalidraw-wopi/internal/wopitest"
)

// These assertions catch drift between room.Manager and the interfaces
// this package wires it into. They live here, not in internal/room, since
// boardapi and relay would otherwise import room and room would import
// them back to declare the reverse assertion.
var (
	_ boardapi.RoomStore     = (*room.Manager)(nil)
	_ boardapi.Observer      = (*room.Manager)(nil)
	_ boardapi.ConflictStore = (*room.Manager)(nil)
	_ relay.RoomEvents       = (*room.Manager)(nil)
)

// roomShutdownFlushTimeout bounds the room Manager's shutdown flush pass.
// http.Server's RegisterOnShutdown callbacks take no context of their
// own (see NewServer's registration below), so this is a fixed,
// self-contained budget rather than one derived from the caller's
// shutdown deadline (cmd/excalidraw-wopi/main.go's own 10s).
const roomShutdownFlushTimeout = 8 * time.Second

// peerTransport, when non-nil, overrides the RoundTripper NewServer hands
// to peers.New. Test seam only (mirrors testhooks.go's env-var seams):
// this package's own two-instance test sets it so a proxied request
// reaches the other replica's mux directly, without a real TCP dial.
var peerTransport http.RoundTripper

// peerResolver, when non-nil, overrides the DNS resolver NewServer hands
// to peers.New. Test seam only: lets a test inject a fake resolver
// instead of dialing DNS for real.
var peerResolver peers.Resolver

// newRoomManagerHook, when non-nil, runs on the room.Manager NewServer
// builds. Test seam only: lets a test reach a specific replica's own
// manager directly, since NewServer otherwise exposes it through no
// public API.
var newRoomManagerHook func(*room.Manager)

// NewServer builds the full HTTP server: discovery, the launch endpoint,
// and the board REST API, sharing one signed WOPI client: every WOPI call
// goes through the Go server.
func NewServer(cfg config.Config, staticFS fs.FS, proofKeys *proof.KeySet) (*http.Server, error) {
	sessions, err := session.New([]byte(cfg.SessionSecret))
	if err != nil {
		return nil, fmt.Errorf("app: build session manager: %w", err)
	}

	var peerOpts []peers.Option
	if peerTransport != nil {
		peerOpts = append(peerOpts, peers.WithTransport(peerTransport))
	}
	if peerResolver != nil {
		peerOpts = append(peerOpts, peers.WithResolver(peerResolver))
	}
	cluster, err := peers.New(peers.Config{DNS: cfg.DNSPeers, Self: cfg.DNSSelf}, peerOpts...)
	if err != nil {
		return nil, fmt.Errorf("app: build peer cluster: %w", err)
	}
	if cluster.Enabled() {
		slog.Info("app: multi-replica routing enabled", "peers", cfg.DNSPeers, "self", cfg.DNSSelf)
	}

	// --fake-host dev mode. allowedOrigins gets the
	// service's own PublicURL added, since the fake launch page posts a
	// WOPISrc that points back at itself; a real deployment never takes
	// this branch (fakeHostAllowed reads its own env var, not config, and
	// additionally refuses to enable on a non-loopback PublicURL).
	allowedOrigins := cfg.AllowedWOPIOrigins
	var fakeHost *wopitest.Host
	var launchOpts []launch.Option
	needTestHooks := false
	if fakeHostAllowed(cfg.PublicURL) {
		logFakeHostWarning()
		fakeHost = newFakeHost()
		allowedOrigins = append(append([]string(nil), cfg.AllowedWOPIOrigins...), cfg.PublicURL)
		// The fake host serves no real user data, so every launch through
		// this instance (fake or, for manual testing, a real WOPISrc) can
		// safely expose window.__excaTest.
		needTestHooks = true
	}
	if testHooksAllowed() {
		logTestHooksWarning()
		needTestHooks = true
	}
	if needTestHooks {
		launchOpts = append(launchOpts, launch.WithTestHooks())
	}

	client := wopiclient.New(nil, timestampedSigner{proofKeys}, hostadapter.NewDrive())
	launchHandler := launch.New(client, sessions, staticFS, allowedOrigins, cfg.MaxImageBytes, launchOpts...)

	// The room Manager replaces boardapi.MemStore: it retains the last
	// posted scene per room the same way, but it also owns the WOPI save
	// loop and the WOPI lock lifecycle. It is both boardapi's
	// RoomStore/ConflictStore and its Observer (the token registry), and
	// it structurally satisfies relay.RoomEvents, so the same value wires
	// into every call site below.
	//
	// room.WithOnTokenExpiring is left at its default (a log line only):
	// wiring the client warn/disconnect emit in here still needs the relay
	// to grow a way to push it to one specific session.
	//
	// rel is declared before rooms and assigned after it (still both
	// before rooms.Start()) to break the circular wiring: rooms needs a
	// relay to broadcast conflict-state changes to, and the relay needs
	// rooms as its RoomEvents. rooms.Start() only runs once rel already
	// holds its final value, so the background loop's onConflictChange
	// call never reads rel before this function has finished assigning it.
	var rel *relay.Relay
	rooms := room.NewManager(client, room.Config{MaxSceneBytes: cfg.MaxSceneBytes}, room.SystemClock,
		room.WithOnConflictChange(func(fileID string, inConflict, saveStalled bool) {
			// An un-Observed PutScene room has no fileID yet, and
			// broadcasting to socket.Room("") would be wrong.
			if fileID == "" {
				return
			}
			rel.BroadcastToRoom(fileID, "conflict-state", conflictStatePayload{InConflict: inConflict, SaveStalled: saveStalled})
		}),
		// ResolveConflict's reload branch drops the room's retained
		// scene, so every client, not just the one that resolved it, must
		// reload to pick up the host's current content (see
		// room.WithOnReloadRequired's doc comment).
		room.WithOnReloadRequired(func(fileID string) {
			if fileID == "" {
				return
			}
			rel.BroadcastToRoom(fileID, "reload-required", struct{}{})
		}),
	)
	if newRoomManagerHook != nil {
		newRoomManagerHook(rooms)
	}

	board := boardapi.New(sessions, client, rooms, cfg.MaxSceneBytes,
		boardapi.WithObserver(rooms), boardapi.WithConflictStore(rooms), boardapi.WithOwnershipCheck(cluster.IsSelf))
	rel = relay.New(&cfg, sessionVerifier{sessions}, relay.WithRoomEvents(rooms), relay.WithOwnershipCheck(cluster.IsSelf))
	rooms.Start()
	cluster.Start()

	srv := httpserver.New(cfg, staticFS, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /hosting/discovery", discovery.Handler(cfg.PublicURL, proofKeys))
		mux.Handle("POST /launch", launchHandler)
		board.RegisterRoutes(mux, cluster.Middleware)
		mux.Handle("/socket.io/", cluster.Middleware(rel.Handler()))
		if fakeHost != nil {
			mountFakeHost(mux, fakeHost, cfg.PublicURL)
		}
	})
	// http.Server.Shutdown waits for hijacked connections (websockets) to
	// close on their own; RegisterOnShutdown runs concurrently with that
	// wait, so the relay's own Close forces them down instead of stalling
	// graceful shutdown until the deadline.
	srv.RegisterOnShutdown(rel.Close)
	srv.RegisterOnShutdown(cluster.Close)
	srv.RegisterOnShutdown(func() {
		ctx, cancel := context.WithTimeout(context.Background(), roomShutdownFlushTimeout)
		defer cancel()
		if err := rooms.Shutdown(ctx); err != nil {
			slog.Error("room: shutdown flush did not complete cleanly", "error", err)
		}
	})

	return srv, nil
}

// conflictStatePayload is the wire shape of the relay's conflict-state
// push, mirrored on the client in web/src/types/collaboration.ts and
// answered identically by boardapi's GET /api/board/conflict
// (internal/boardapi's conflictStateResponse).
type conflictStatePayload struct {
	InConflict  bool `json:"inConflict"`
	SaveStalled bool `json:"saveStalled"`
}

// sessionVerifier adapts session.Manager to relay.TokenVerifier. It drops
// the unsealed WOPI access token: the relay must never hold it.
type sessionVerifier struct {
	sessions *session.Manager
}

func (v sessionVerifier) Verify(raw string) (relay.Session, error) {
	claims, err := v.sessions.Verify(raw)
	if err != nil {
		return relay.Session{}, err
	}
	return relay.Session{
		FileID:   claims.FileID,
		UserID:   claims.UserID,
		UserName: claims.UserName,
		CanWrite: claims.CanWrite,
	}, nil
}

// timestampedSigner adapts proof.Signer, which takes an explicit
// timestamp so its signing math stays testable against fixed ticks
// values, to wopiclient.RequestSigner, which does not; it supplies
// time.Now() at call time.
type timestampedSigner struct {
	signer proof.Signer
}

func (s timestampedSigner) Sign(accessToken, url string) (sig, sigOld, timestamp string) {
	return s.signer.SignRequest(accessToken, url, time.Now())
}
