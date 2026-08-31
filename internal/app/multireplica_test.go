package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/config"
	"github.com/zeylos/excalidraw-wopi/internal/peers"
	"github.com/zeylos/excalidraw-wopi/internal/proof"
	"github.com/zeylos/excalidraw-wopi/internal/room"
	"github.com/zeylos/excalidraw-wopi/internal/session"
)

// fixedResolver is a peers.Resolver that always answers with the same
// fixed address list, regardless of the host asked for. It stands in for
// a real DNS lookup in a sandbox that cannot dial out.
type fixedResolver struct {
	addrs []net.IPAddr
}

func (r fixedResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return r.addrs, nil
}

// routedTransport is the http.RoundTripper the two-instance test injects
// through the peerTransport test seam. It dispatches a proxied request
// straight to the target replica's own mux via httptest, keyed on the
// target host peers.Middleware's ReverseProxy sets on the outbound
// request's URL, so the test never dials a real TCP socket (some
// sandboxes refuse loopback dials).
type routedTransport struct {
	mu       sync.Mutex
	handlers map[string]http.Handler
	sawHop   bool
}

func (rt *routedTransport) register(host string, h http.Handler) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.handlers == nil {
		rt.handlers = make(map[string]http.Handler)
	}
	rt.handlers[host] = h
}

func (rt *routedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	h, ok := rt.handlers[req.URL.Host]
	if req.Header.Get("X-Excalidraw-Wopi-Hop") != "" {
		rt.sawHop = true
	}
	rt.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("routedTransport: no handler registered for host %q", req.URL.Host)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// dnsClusterPeers is the fixed two-address answer the fake resolver hands
// back for every lookup in this test: one dns spec, one port, both
// replicas reachable through it.
const dnsClusterPeers = "svc.local:8080"

// multiReplicaConfig builds a config.Config for one replica of a
// two-instance dns cluster. Both replicas must share secret, so a session
// token minted against either verifies on the other.
func multiReplicaConfig(t *testing.T, self, secret string) config.Config {
	t.Helper()
	return config.Config{
		ListenAddr:        ":0",
		PublicURL:         "https://excalidraw.example.org",
		SessionSecret:     secret,
		ProofKeyPath:      filepath.Join(t.TempDir(), "proof-key.pem"),
		MaxImageBytes:     10 * 1024 * 1024,
		MaxSceneBytes:     50 * 1024 * 1024,
		SocketBufferBytes: 60 * 1024 * 1024,
		DNSPeers:          dnsClusterPeers,
		DNSSelf:           self,
	}
}

// waitForReplicaReady polls until mux's own Cluster state has picked up
// the full peer set. It drives GET /api/board through mux for a
// candidate room probe predicts wantOtherOwner owns, and retries until
// the response shows a routing decision: either a proxied hop, read off
// transport's sawHop flag, or a 421 (peers disagreeing on ownership).
// Before mux's own DNS poll resolves, Owner always falls back to self,
// so mux serves such a request locally instead of proxying or rejecting
// it, and neither signal appears yet.
func waitForReplicaReady(t *testing.T, mux http.Handler, transport *routedTransport, probe *peers.Cluster, wantOtherOwner string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for i := range 200 {
			candidate := fmt.Sprintf("resolve-probe-%d", i)
			if probe.Owner(candidate) != wantOtherOwner {
				continue
			}

			transport.mu.Lock()
			transport.sawHop = false
			transport.mu.Unlock()

			req := httptest.NewRequest(http.MethodGet, "/api/board?room="+candidate, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			transport.mu.Lock()
			proxied := transport.sawHop
			transport.mu.Unlock()

			if proxied || rec.Code == http.StatusMisdirectedRequest {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("replica did not resolve peers within the deadline")
}

// findRoomOwnedBy searches small integer-suffixed room ids for one c maps
// to wantOwner, the same technique internal/peers' own tests use to
// exercise a specific rendezvous outcome deterministically.
func findRoomOwnedBy(t *testing.T, c *peers.Cluster, wantOwner string) string {
	t.Helper()
	for i := range 1000 {
		candidate := fmt.Sprintf("file-%d", i)
		if c.Owner(candidate) == wantOwner {
			return candidate
		}
	}
	t.Fatalf("could not find a room id owned by %q", wantOwner)
	return ""
}

// TestTwoReplicaClusterProxiesToOwnerAndEnforcesTokenOwnership builds two
// full servers sharing one dns peer spec and one session secret, wired
// together through an in-memory transport (no sockets), and checks the
// three multi-replica invariants: a write against the non-owner is
// proxied to the owner rather than rejected, a room hint that disagrees
// with the token's fileID is rejected before it ever reaches the room
// observer, and a proxied request carries the hop guard header.
func TestTwoReplicaClusterProxiesToOwnerAndEnforcesTokenOwnership(t *testing.T) {
	const secret = "a test secret with enough entropy"
	const selfA = "http://10.0.0.1:8080"
	const selfB = "http://10.0.0.2:8080"

	resolver := fixedResolver{addrs: []net.IPAddr{
		{IP: net.ParseIP("10.0.0.1")},
		{IP: net.ParseIP("10.0.0.2")},
	}}

	// peerTransport, peerResolver, and newRoomManagerHook are
	// unsynchronized package vars (test seams only): this test, and any
	// other in package app, must not run under t.Parallel().
	transport := &routedTransport{}
	peerTransport = transport
	t.Cleanup(func() { peerTransport = nil })
	peerResolver = resolver
	t.Cleanup(func() { peerResolver = nil })

	staticFS := fstest.MapFS{
		"index.html": {Data: []byte(`<script type="application/json" id="ew-config">{}</script>`)},
	}

	var roomManagerA, roomManagerB *room.Manager
	t.Cleanup(func() { newRoomManagerHook = nil })

	cfgA := multiReplicaConfig(t, selfA, secret)
	proofKeysA, err := proof.Load(cfgA)
	if err != nil {
		t.Fatalf("proof.Load(A) error = %v", err)
	}
	newRoomManagerHook = func(rm *room.Manager) { roomManagerA = rm }
	srvA, err := NewServer(cfgA, staticFS, proofKeysA)
	if err != nil {
		t.Fatalf("NewServer(A) error = %v", err)
	}

	cfgB := multiReplicaConfig(t, selfB, secret)
	proofKeysB, err := proof.Load(cfgB)
	if err != nil {
		t.Fatalf("proof.Load(B) error = %v", err)
	}
	newRoomManagerHook = func(rm *room.Manager) { roomManagerB = rm }
	srvB, err := NewServer(cfgB, staticFS, proofKeysB)
	if err != nil {
		t.Fatalf("NewServer(B) error = %v", err)
	}

	transport.register("10.0.0.1:8080", srvA.Handler)
	transport.register("10.0.0.2:8080", srvB.Handler)
	roomManagerByURL := map[string]*room.Manager{
		selfA: roomManagerA,
		selfB: roomManagerB,
	}

	// A third, independent Cluster computes ownership the same way both
	// replicas do, purely as a function of (fileID, peer set). It shares
	// the fixed resolver, so its own resolve settles the same snapshot A
	// and B's already-running polls do; it exists only to predict which
	// replica a candidate room hashes to, for waitForReplicaReady and
	// findRoomOwnedBy below. Readiness itself is checked against A and
	// B's own handlers, not inferred from probe's resolve.
	probe, err := peers.New(peers.Config{DNS: dnsClusterPeers, Self: selfA}, peers.WithResolver(resolver))
	if err != nil {
		t.Fatalf("peers.New(probe) error = %v", err)
	}
	probe.Start()
	t.Cleanup(probe.Close)

	waitForReplicaReady(t, srvA.Handler, transport, probe, selfB)
	waitForReplicaReady(t, srvB.Handler, transport, probe, selfA)
	// Leave the shared transport's hop flag clean for the subtests below,
	// which read it fresh after their own action.
	transport.mu.Lock()
	transport.sawHop = false
	transport.mu.Unlock()

	muxByURL := map[string]http.Handler{
		selfA: srvA.Handler,
		selfB: srvB.Handler,
	}

	fileID := findRoomOwnedBy(t, probe, selfA)
	ownerURL := probe.Owner(fileID)
	var nonOwnerURL string
	for u := range muxByURL {
		if u != ownerURL {
			nonOwnerURL = u
		}
	}
	ownerMux := muxByURL[ownerURL]
	nonOwnerMux := muxByURL[nonOwnerURL]

	sessions, err := session.New([]byte(secret))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	const wopiSrc = "https://drive.example/files/" + "f1"
	token, err := sessions.Mint(session.MintParams{
		FileID:      fileID,
		WOPISrc:     wopiSrc,
		UserID:      "u1",
		UserName:    "Alice",
		CanWrite:    true,
		AccessToken: "drive-access-token",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	const scene = `{"elements":[{"id":"e1"}]}`

	t.Run("PutOnNonOwnerIsProxiedToOwner", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/api/board?room="+fileID, strings.NewReader(scene))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		nonOwnerMux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("PUT against non-owner status = %d, want %d; body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
		}

		transport.mu.Lock()
		sawHop := transport.sawHop
		transport.mu.Unlock()
		if !sawHop {
			t.Error("proxied request never carried the X-Excalidraw-Wopi-Hop header")
		}

		getReq := httptest.NewRequest(http.MethodGet, "/api/board?room="+fileID, nil)
		getReq.Header.Set("Authorization", "Bearer "+token)
		getRec := httptest.NewRecorder()
		ownerMux.ServeHTTP(getRec, getReq)
		if getRec.Code != http.StatusOK {
			t.Fatalf("GET against owner status = %d, want %d; body: %s", getRec.Code, http.StatusOK, getRec.Body.String())
		}
		body, err := io.ReadAll(getRec.Body)
		if err != nil {
			t.Fatalf("read GET body: %v", err)
		}
		if string(body) != scene {
			t.Errorf("GET body = %q, want %q", body, scene)
		}
	})

	t.Run("MismatchedRoomHintIsRejectedAs421", func(t *testing.T) {
		// A room hint the target replica owns, so peers.Middleware passes
		// the request straight through instead of proxying it; the token
		// still names fileID, which this replica does not own, so the
		// rejection has to come from boardapi's own ownership check.
		hintFileID := findRoomOwnedBy(t, probe, nonOwnerURL)

		req := httptest.NewRequest(http.MethodGet, "/api/board?room="+hintFileID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		nonOwnerMux.ServeHTTP(rec, req)

		if rec.Code != http.StatusMisdirectedRequest {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusMisdirectedRequest, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "wrong replica") {
			t.Errorf("body = %q, want it to mention %q", rec.Body.String(), "wrong replica")
		}

		// The rejection must land before Observe: the non-owner's own room
		// manager must hold no room at all for wopiSrc, proving it never
		// registered the token.
		if _, ok := roomManagerByURL[nonOwnerURL].GetScene(wopiSrc); ok {
			t.Error("non-owner replica holds a room for a file it does not own; the ownership check ran after Observe")
		}
	})
}
