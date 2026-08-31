// Command wopihost runs internal/wopitest's in-memory WOPI host as a
// standalone process for the HA harness in e2e/ha. Unlike
// internal/app/fakehost.go, which mounts the same host inside the
// service binary for dev mode, this process runs on its own, outliving
// every excalidraw-wopi instance the harness starts and kills, so a test
// can read the stored file body and lock state after an instance dies.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/wopitest"
)

const (
	basePath    = "/wopi/files"
	defaultAddr = "127.0.0.1:18866"
)

// seedResponse is what POST /seed answers: the WOPISrc a launch call
// needs, and one access token per requested user.
type seedResponse struct {
	FileID  string            `json:"fileId"`
	WOPISrc string            `json:"wopiSrc"`
	Tokens  map[string]string `json:"tokens"`
}

// stateResponse is what GET /state answers: fileID's stored bytes,
// version, save count, and current lock, so a test can poll for a save
// or a failover re-lock instead of guessing a sleep.
type stateResponse struct {
	Size     int64  `json:"size"`
	Version  string `json:"version"`
	PutCount int    `json:"putCount"`
	Lock     string `json:"lock"`
	Content  string `json:"content"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run keeps the fatal exit out of the function that owns defers, so
// stop() still runs on the error paths.
func run() error {
	addr := envOr("HA_WOPIHOST_ADDR", defaultAddr)
	host := wopitest.New(basePath, hostadapter.LockTTL)

	mux := http.NewServeMux()
	mux.Handle(basePath+"/", host.Handler())
	mux.HandleFunc("POST /seed", handleSeed(host, addr))
	mux.HandleFunc("GET /state", handleState(host))

	srv := &http.Server{Addr: addr, Handler: mux}

	// Listen before reporting ready, so a busy port fails fast and names
	// itself instead of the harness waiting out its startup timeout.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("wopihost: listen on %s: %w", addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	// The harness's global-setup waits on this exact substring on stdout
	// to know the host is ready; log defaults to stderr, so this line
	// goes to stdout explicitly.
	fmt.Printf("ha wopihost listening on %s\n", addr)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// handleSeed registers a fresh file plus one User and access token per
// name in the "writers" and "readers" query parameters (comma-separated
// user ids), and answers the WOPISrc and the minted tokens. A missing
// "file" parameter gets a random id, so a test that needs many files for
// a hash-distribution check does not have to invent ids itself.
func handleSeed(host *wopitest.Host, addr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := r.URL.Query().Get("file")
		if fileID == "" {
			var err error
			fileID, err = randomID()
			if err != nil {
				http.Error(w, "wopihost: generate file id: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}

		writers := splitCSV(r.URL.Query().Get("writers"))
		readers := splitCSV(r.URL.Query().Get("readers"))
		if len(writers) == 0 && len(readers) == 0 {
			http.Error(w, "wopihost: /seed needs at least one of writers= or readers=", http.StatusBadRequest)
			return
		}

		host.AddFile(fileID, fileID+".excalidraw", firstOr(writers, readers), nil)

		tokens := make(map[string]string, len(writers)+len(readers))
		for _, id := range writers {
			host.AddUser(wopitest.User{ID: id, Name: id, CanWrite: true})
			tokens[id] = host.MintToken(id, fileID)
		}
		for _, id := range readers {
			host.AddUser(wopitest.User{ID: id, Name: id, CanWrite: false})
			tokens[id] = host.MintToken(id, fileID)
		}

		writeJSON(w, seedResponse{
			FileID:  fileID,
			WOPISrc: "http://" + addr + basePath + "/" + fileID,
			Tokens:  tokens,
		})
	}
}

// handleState answers fileID's live state for a test's poll loop.
func handleState(host *wopitest.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := r.URL.Query().Get("file")
		stats, ok := host.Stats(fileID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		lock, _ := host.Lock(fileID)
		content, _ := host.Content(fileID)

		writeJSON(w, stateResponse{
			Size:     stats.Size,
			Version:  stats.Version,
			PutCount: stats.PutCount,
			Lock:     lock,
			Content:  base64.StdEncoding.EncodeToString(content),
		})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// firstOr returns the first entry of a, or of b when a is empty, or "" when
// both are empty. It picks AddFile's ownerID from whichever of writers or
// readers the seed call actually supplied.
func firstOr(a, b []string) string {
	if len(a) > 0 {
		return a[0]
	}
	if len(b) > 0 {
		return b[0]
	}
	return ""
}

func randomID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "ha-" + hex.EncodeToString(buf), nil
}

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}
