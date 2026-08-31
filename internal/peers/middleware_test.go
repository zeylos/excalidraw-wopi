package peers

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

// newFixedCluster builds an enabled Cluster with a fixed peer snapshot,
// bypassing DNS resolution. A middleware test needs peers on distinct
// loopback ports, which a dns spec cannot express: it carries one port
// for every resolved peer.
func newFixedCluster(t *testing.T, self string, peersList []string) *Cluster {
	t.Helper()
	sorted := append([]string(nil), peersList...)
	sort.Strings(sorted)
	c := &Cluster{
		enabled:   true,
		self:      self,
		transport: newTransport(),
	}
	c.snapshot.Store(&sorted)
	return c
}

// requireLoopbackDial skips the test when the environment refuses a
// loopback TCP dial, so a test that needs a real listener degrades to a
// skip instead of a hang or a failure unrelated to the code under test.
func requireLoopbackDial(t *testing.T) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("skipping: cannot listen on loopback TCP: %v", err)
	}
	defer lis.Close()

	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Skipf("skipping: cannot dial loopback TCP in this environment: %v", err)
	}
	conn.Close()
}

func TestMiddlewarePassesThroughWhenDisabled(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New() returned an error: %v", err)
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x?room=anything", nil)
	c.Middleware(next).ServeHTTP(rec, req)

	if !called {
		t.Error("Middleware must pass through to next when the cluster is disabled")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMiddlewarePassesThroughWhenRoomIsEmpty(t *testing.T) {
	c := newFixedCluster(t, "http://a", []string{"http://a", "http://b"})

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Middleware(next).ServeHTTP(rec, req)

	if !called {
		t.Error("Middleware must pass through to next when the room query parameter is absent")
	}
}

func TestMiddlewarePassesThroughWhenSelfOwnsRoom(t *testing.T) {
	// Two peers: Owner() picks self only for some room ids, so the loop
	// below searches for one instead of assuming every room is self-owned.
	c := newFixedCluster(t, "http://self", []string{"http://self", "http://other"})

	// Find a room string this cluster owns.
	var room string
	for i := 0; ; i++ {
		candidate := fmt.Sprintf("room-%d", i)
		if c.IsSelf(candidate) {
			room = candidate
			break
		}
		if i > 1000 {
			t.Fatal("could not find a self-owned room id")
		}
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x?room="+room, nil)
	c.Middleware(next).ServeHTTP(rec, req)

	if !called {
		t.Error("Middleware must pass through to next when this replica owns the room")
	}
}

func TestMiddlewareProxiesForeignRoomToOwner(t *testing.T) {
	requireLoopbackDial(t)

	peerBody := "hello from the peer"
	peerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(hopHeader) != "1" {
			t.Errorf("peer received request without hop header: %v", r.Header)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(peerBody))
	}))
	defer peerServer.Close()

	c := newFixedCluster(t, "http://self.invalid", []string{peerServer.URL, "http://self.invalid"})

	room := foreignRoom(t, c, peerServer.URL)

	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x?room="+room, nil)
	c.Middleware(next).ServeHTTP(rec, req)

	if called {
		t.Error("Middleware must not call next for a foreign room; it must proxy instead")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != peerBody {
		t.Errorf("body = %q, want %q", body, peerBody)
	}
}

func TestMiddlewareIgnoresClientSuppliedHopHeaderOnFirstHop(t *testing.T) {
	requireLoopbackDial(t)

	peerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Values(hopHeader); len(got) != 1 || got[0] != "1" {
			t.Errorf("peer received hop header %v, want exactly one value %q", got, "1")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer peerServer.Close()

	c := newFixedCluster(t, "http://self.invalid", []string{peerServer.URL, "http://self.invalid"})

	room := foreignRoom(t, c, peerServer.URL)

	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("Middleware must not call next for a foreign room")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x?room="+room, nil)
	req.Header.Set(hopHeader, "spoofed") // a client must not force a 421 this way.
	c.Middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMiddlewareAnswers421WhenHopHeaderPresentAndStillForeign(t *testing.T) {
	c := newFixedCluster(t, "http://self.invalid", []string{"http://peer.invalid", "http://self.invalid"})

	room := foreignRoom(t, c, "http://peer.invalid")

	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("Middleware must not call next on a second hop")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x?room="+room, nil)
	req.Header.Set(hopHeader, "1")
	c.Middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMisdirectedRequest)
	}
}

func TestMiddlewareAnswers502WhenOwnerIsDown(t *testing.T) {
	requireLoopbackDial(t)

	downServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	downURL := downServer.URL
	downServer.Close() // keep the URL, but leave nothing listening on it.

	c := newFixedCluster(t, "http://self.invalid", []string{downURL, "http://self.invalid"})

	room := foreignRoom(t, c, downURL)

	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x?room="+room, nil)
	c.Middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

// foreignRoom finds a room id this cluster maps to wantOwner, so a test can
// exercise the proxy path deterministically.
func foreignRoom(t *testing.T, c *Cluster, wantOwner string) string {
	t.Helper()
	for i := range 1000 {
		candidate := fmt.Sprintf("room-%d", i)
		if c.Owner(candidate) == wantOwner {
			return candidate
		}
	}
	t.Fatalf("could not find a room id owned by %q", wantOwner)
	return ""
}
