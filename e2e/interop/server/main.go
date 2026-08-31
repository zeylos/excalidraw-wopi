// Command main runs a disposable relay instance for the interop harness in
// e2e/interop. Unlike internal/relay/smoke, this one is a normal build
// target: the harness's globalSetup builds and starts it once per `make
// interop` run.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/zeylos/excalidraw-wopi/internal/config"
	"github.com/zeylos/excalidraw-wopi/internal/relay"
)

const (
	interopRoomID           = "interop-room"
	otherRoomID             = "other-room"
	defaultAddr             = "127.0.0.1:18765"
	defaultMaxSceneBytes    = 1 * 1024 * 1024
	defaultSocketBufferSize = 8 * 1024 * 1024
)

// interopSessions maps every token the harness's socket.io-client sockets
// use to the session it carries. writer-a and writer-b share interopRoomID
// with write access; reader shares it read-only. other-room-writer claims
// a different fileId, so a join-room "interop-room" attempt with that token
// exercises the room-does-not-match-claim rejection.
var interopSessions = map[string]relay.Session{
	"writer-a":          {FileID: interopRoomID, UserID: "writer-a", UserName: "Writer A", CanWrite: true},
	"writer-b":          {FileID: interopRoomID, UserID: "writer-b", UserName: "Writer B", CanWrite: true},
	"reader":            {FileID: interopRoomID, UserID: "reader-1", UserName: "Reader", CanWrite: false},
	"other-room-writer": {FileID: otherRoomID, UserID: "other-writer", UserName: "Other Writer", CanWrite: true},
}

type interopVerifier struct{}

func (interopVerifier) Verify(raw string) (relay.Session, error) {
	sess, ok := interopSessions[raw]
	if !ok {
		return relay.Session{}, errors.New("unknown token")
	}
	return sess, nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run keeps the fatal exit out of the function that owns defers, so
// rel.Close and stop() still run on the error paths.
func run() error {
	addr := envOr("INTEROP_ADDR", defaultAddr)
	maxSceneBytes := envInt64Or("INTEROP_MAX_SCENE_BYTES", defaultMaxSceneBytes)
	socketBufferBytes := envInt64Or("INTEROP_SOCKET_BUFFER_BYTES", defaultSocketBufferSize)

	rel := relay.New(&config.Config{SocketBufferBytes: socketBufferBytes, MaxSceneBytes: maxSceneBytes}, interopVerifier{})
	defer rel.Close()

	mux := http.NewServeMux()
	mux.Handle("/socket.io/", rel.Handler())
	srv := &http.Server{Addr: addr, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	// The harness's globalSetup waits on this exact substring on stdout to
	// know the server is ready to accept connections; log defaults to
	// stderr, so this line goes to stdout explicitly.
	fmt.Printf("interop server listening on %s (scene limit %d bytes)\n", addr, maxSceneBytes)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

func envInt64Or(name string, fallback int64) int64 {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Fatalf("%s: invalid integer %q: %v", name, v, err)
	}
	return n
}
