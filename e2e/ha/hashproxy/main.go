// Command hashproxy is a small reverse proxy for the HA harness in
// e2e/ha. It fans a "room" query parameter out to a fixed backend list
// with a rendezvous hash, the same shape internal/peers' Cluster.Owner
// uses for replica ownership, so every request naming one room lands on
// one backend for as long as that backend stays healthy. A request with
// no "room" parameter goes to the first healthy backend, in the order
// HA_PROXY_BACKENDS lists them.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultAddr    = "127.0.0.1:18868"
	healthInterval = 500 * time.Millisecond
	healthTimeout  = 800 * time.Millisecond
	// ejectAfterFailures absorbs one slow-but-alive probe (a loaded build
	// host, a GC pause) without ejecting a healthy backend for it alone.
	ejectAfterFailures = 2
)

// targetKey carries the request's chosen backend URL from ServeHTTP to
// the shared reverse proxy's Rewrite callback, the same pattern
// internal/peers/middleware.go uses for its own per-request target.
type targetKey struct{}

// router picks a backend per request and reverse-proxies to it.
// httputil.ReverseProxy passes a websocket upgrade through unchanged, so
// the same router serves both the socket.io handshake and the plain REST
// calls.
type router struct {
	backends []string // fixed order; a room-less request picks the first healthy entry

	mu      sync.RWMutex
	healthy map[string]bool

	proxy *httputil.ReverseProxy
}

func newRouter(backends []string) *router {
	rtr := &router{backends: backends, healthy: make(map[string]bool, len(backends))}
	for _, b := range backends {
		rtr.healthy[b] = true // assumed healthy until the first poll says otherwise
	}
	rtr.proxy = &httputil.ReverseProxy{
		Rewrite:       rtr.rewrite,
		FlushInterval: -1, // keeps ReverseProxy's native websocket upgrade support working
		ErrorHandler:  rtr.errorHandler,
	}
	return rtr
}

func (rtr *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__owner" {
		rtr.handleOwner(w, r)
		return
	}

	backend := rtr.pick(r.URL.Query().Get("room"))
	if backend == "" {
		http.Error(w, "hashproxy: no healthy backend", http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(backend)
	if err != nil {
		http.Error(w, "hashproxy: unparsable backend URL", http.StatusInternalServerError)
		return
	}
	ctx := context.WithValue(r.Context(), targetKey{}, target)
	rtr.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (rtr *router) rewrite(pr *httputil.ProxyRequest) {
	target, _ := pr.In.Context().Value(targetKey{}).(*url.URL)
	pr.SetURL(target)
	pr.Out.Host = pr.In.Host
}

func (rtr *router) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	target, _ := r.Context().Value(targetKey{}).(*url.URL)
	log.Printf("hashproxy: proxy request to %v failed: %v", target, err)
	http.Error(w, "hashproxy: backend request failed", http.StatusBadGateway)
}

// handleOwner answers GET /__owner?room=<id> with the backend the current
// health snapshot picks for room, so a test knows which instance to
// SIGKILL for a failover scenario.
func (rtr *router) handleOwner(w http.ResponseWriter, r *http.Request) {
	room := r.URL.Query().Get("room")
	if room == "" {
		http.Error(w, "hashproxy: /__owner needs a room parameter", http.StatusBadRequest)
		return
	}
	backend := rtr.pick(room)
	if backend == "" {
		http.Error(w, "hashproxy: no healthy backend", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Owner string `json:"owner"`
	}{Owner: backend})
}

// pick returns the backend room routes to, among the currently healthy
// ones, or "" when none are healthy. An empty room picks the first
// healthy backend in rtr.backends order.
func (rtr *router) pick(room string) string {
	rtr.mu.RLock()
	defer rtr.mu.RUnlock()

	var candidates []string
	for _, b := range rtr.backends {
		if rtr.healthy[b] {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	if room == "" {
		return candidates[0]
	}
	return rendezvousOwner(room, candidates)
}

// rendezvousOwner picks the candidate with the highest rendezvous-hash
// score for room, breaking a tie on the lexicographically greater
// backend URL so the pick stays deterministic.
func rendezvousOwner(room string, candidates []string) string {
	best := ""
	var bestScore uint64
	for _, c := range candidates {
		score := rendezvousScore(room, c)
		if best == "" || score > bestScore || (score == bestScore && c > best) {
			best = c
			bestScore = score
		}
	}
	return best
}

func rendezvousScore(room, backend string) uint64 {
	h := sha256.Sum256([]byte(room + "\x00" + backend))
	return binary.BigEndian.Uint64(h[:8])
}

// healthLoop polls every backend's /healthz on healthInterval. A backend
// is ejected only after ejectAfterFailures consecutive failed probes,
// and readmitted on its very next successful one.
func (rtr *router) healthLoop(ctx context.Context) {
	client := &http.Client{Timeout: healthTimeout}
	failures := make(map[string]int, len(rtr.backends))
	check := func() {
		results := make([]bool, len(rtr.backends))
		var wg sync.WaitGroup
		for i, b := range rtr.backends {
			wg.Add(1)
			go func(i int, b string) {
				defer wg.Done()
				results[i] = probeHealthy(client, b)
			}(i, b)
		}
		wg.Wait()

		rtr.mu.Lock()
		for i, b := range rtr.backends {
			if results[i] {
				failures[b] = 0
				rtr.healthy[b] = true
				continue
			}
			failures[b]++
			if failures[b] >= ejectAfterFailures {
				rtr.healthy[b] = false
			}
		}
		rtr.mu.Unlock()
	}

	check()
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func probeHealthy(client *http.Client, backend string) bool {
	resp, err := client.Get(backend + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run keeps the fatal exit out of the function that owns defers, so
// stop() still runs on the error paths.
func run() error {
	addr := envOr("HA_PROXY_ADDR", defaultAddr)
	backends := splitCSV(os.Getenv("HA_PROXY_BACKENDS"))
	if len(backends) == 0 {
		return errors.New("hashproxy: HA_PROXY_BACKENDS must list at least one backend URL")
	}

	rtr := newRouter(backends)

	// Listen before reporting ready, so a busy port fails fast and names
	// itself instead of the harness waiting out its startup timeout.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("hashproxy: listen on %s: %w", addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go rtr.healthLoop(ctx)

	srv := &http.Server{Addr: addr, Handler: rtr}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	// The harness's global-setup waits on this exact substring on stdout
	// to know the proxy is ready; log defaults to stderr, so this line
	// goes to stdout explicitly.
	fmt.Printf("ha hashproxy listening on %s (backends %s)\n", addr, strings.Join(backends, ","))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}
