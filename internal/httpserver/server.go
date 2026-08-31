// Package httpserver builds the HTTP server that serves the API and the SPA.
package httpserver

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/config"
)

// readHeaderTimeout bounds how long the server waits for a client to
// finish sending request headers, so a slow-header connection cannot tie
// up a goroutine indefinitely. WriteTimeout stays unset: a websocket
// connection (/socket.io/) legitimately stays open far longer than any
// fixed write deadline would allow.
const readHeaderTimeout = 10 * time.Second

// readTimeout bounds how long the server waits for a client to finish
// sending the full request, headers and body together, so a slow-body
// upload cannot tie up a connection indefinitely. It stays well above the
// board API's largest expected request (a scene PUT), so it never cuts
// off a legitimate slow client.
const readTimeout = 30 * time.Second

// idleTimeout bounds how long the server keeps an idle keep-alive
// connection open between requests, so a client that opens a connection
// and never closes it cannot exhaust the server's file descriptors.
const idleTimeout = 120 * time.Second

// New builds the HTTP server. It registers the health check, the discovery
// endpoint, and the static SPA handler, and it leaves the mux open for
// later route packages to add the launch endpoint, the WOPI proxy, and
// /socket.io/.
func New(cfg config.Config, staticFS fs.FS, extra ...func(*http.ServeMux)) *http.Server {
	mux := http.NewServeMux()
	RegisterRoutes(mux, staticFS)
	for _, register := range extra {
		register(mux)
	}

	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// RegisterRoutes wires the core routes onto mux. New's extra callbacks
// register further handlers on the same mux, to add the launch
// endpoint, the WOPI proxy, and /socket.io/.
func RegisterRoutes(mux *http.ServeMux, staticFS fs.FS) {
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("/", newSPAHandler(staticFS))
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// spaHandler serves static files from an embedded FS. A request path
// without a file extension falls back to index.html, so client-side
// routes in the SPA resolve on a full page load.
type spaHandler struct {
	fileServer http.Handler
	fs         fs.FS
}

func newSPAHandler(staticFS fs.FS) http.Handler {
	return &spaHandler{
		fileServer: http.FileServer(http.FS(staticFS)),
		fs:         staticFS,
	}
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))

	if clean == "." {
		http.ServeFileFS(w, r, h.fs, "index.html")
		return
	}

	if info, err := fs.Stat(h.fs, clean); err == nil {
		if info.IsDir() {
			http.NotFound(w, r)
			return
		}
		h.fileServer.ServeHTTP(w, r)
		return
	}

	if path.Ext(clean) != "" {
		http.NotFound(w, r)
		return
	}

	http.ServeFileFS(w, r, h.fs, "index.html")
}
