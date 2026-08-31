package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zeylos/excalidraw-wopi/internal/app"
	"github.com/zeylos/excalidraw-wopi/internal/config"
	"github.com/zeylos/excalidraw-wopi/internal/proof"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		ListenAddr:        ":0",
		PublicURL:         "https://excalidraw.example.org",
		SessionSecret:     "a test secret with enough entropy",
		ProofKeyPath:      filepath.Join(t.TempDir(), "proof-key.pem"),
		MaxImageBytes:     10 * 1024 * 1024,
		MaxSceneBytes:     50 * 1024 * 1024,
		SocketBufferBytes: 60 * 1024 * 1024,
	}
}

// testFakeHostConfig is testConfig with a loopback PublicURL: fake-host
// mode refuses to enable on anything else, so every test that actually
// exercises the mounted fake host needs this instead of testConfig's
// real-looking, non-loopback PublicURL.
func testFakeHostConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.PublicURL = "http://127.0.0.1:8080"
	return cfg
}

// TestNewServerWiresRoutes is a smoke test: it does not re-exercise each
// route's behavior (that lives in the owning package's tests), only that
// NewServer wires discovery, /launch, and the board API onto one mux.
func TestNewServerWiresRoutes(t *testing.T) {
	cfg := testConfig(t)
	proofKeys, err := proof.Load(cfg)
	if err != nil {
		t.Fatalf("proof.Load() error = %v", err)
	}
	staticFS := fstest.MapFS{
		"index.html": {Data: []byte(`<script type="application/json" id="ew-config">{}</script>`)},
	}

	srv, err := app.NewServer(cfg, staticFS, proofKeys)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		method     string
		path       string
		wantStatus int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/hosting/discovery", http.StatusOK},
		{http.MethodPost, "/launch", http.StatusBadRequest}, // no WOPISrc: still routed, rejected by launch's own validation
		{http.MethodGet, "/api/board", http.StatusUnauthorized},
		{http.MethodPut, "/api/board", http.StatusUnauthorized},
		{http.MethodGet, "/socket.io/?EIO=4&transport=polling", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(""))
			rec := httptest.NewRecorder()
			srv.Handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestFakeHostModeDisabledByDefault checks that NewServer mounts none of
// the /fakewopi/ routes unless EXCALIDRAW_WOPI_FAKE_HOST=1: a production
// deployment must never expose them by accident. The SPA catch-all route
// (RegisterRoutes registers "/" for every method) answers any
// unregistered path with a 200 and index.html, extension-less paths
// included, so the signal is the body, not the status: the fake launch
// page's access_token form field must be absent.
func TestFakeHostModeDisabledByDefault(t *testing.T) {
	cfg := testConfig(t)
	proofKeys, err := proof.Load(cfg)
	if err != nil {
		t.Fatalf("proof.Load() error = %v", err)
	}
	staticFS := fstest.MapFS{
		"index.html": {Data: []byte(`<script type="application/json" id="ew-config">{}</script>`)},
	}
	srv, err := app.NewServer(cfg, staticFS, proofKeys)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/fakewopi/launch?user=alice", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "access_token") {
		t.Errorf("GET /fakewopi/launch served the fake launch page while the fake host is not enabled: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/fakewopi/_state", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "putCount") {
		t.Errorf("GET /fakewopi/_state served fake-host state while the fake host is not enabled: %s", rec.Body.String())
	}
}

// TestFakeHostMode drives the mounted fake host through plain HTTP, the
// way a browser or a Playwright test does: fetch the launch page for
// each user, pull the token it minted out of the form, and use it
// directly against the mounted /fakewopi/files/ routes and the
// /fakewopi/_state introspection endpoint.
func TestFakeHostMode(t *testing.T) {
	t.Setenv("EXCALIDRAW_WOPI_FAKE_HOST", "1")

	cfg := testFakeHostConfig(t)
	proofKeys, err := proof.Load(cfg)
	if err != nil {
		t.Fatalf("proof.Load() error = %v", err)
	}
	staticFS := fstest.MapFS{
		"index.html": {Data: []byte(`<script type="application/json" id="ew-config">{}</script>`)},
	}
	srv, err := app.NewServer(cfg, staticFS, proofKeys)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	mux := srv.Handler

	t.Run("UnknownUserRejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/fakewopi/launch?user=carol", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("WriterCanReadAndWrite", func(t *testing.T) {
		token, wopiSrc := fakeLaunch(t, mux, "alice")
		if want := cfg.PublicURL + "/fakewopi/files/f-local"; wopiSrc != want {
			t.Errorf("WOPISrc = %q, want %q", wopiSrc, want)
		}

		info := fakeCheckFileInfo(t, mux, token)
		if !info.UserCanWrite {
			t.Error("UserCanWrite = false for alice, want true")
		}

		// Version is an S3-style ETag (content hash): md5("") for a
		// fresh, still-empty file.
		stats := fakeState(t, mux)
		if want := "d41d8cd98f00b204e9800998ecf8427e"; stats.Size != 0 || stats.Version != want || stats.PutCount != 0 {
			t.Errorf("initial /fakewopi/_state = %+v, want size 0 / version %q / putCount 0", stats, want)
		}
	})

	t.Run("ReaderCannotWrite", func(t *testing.T) {
		token, _ := fakeLaunch(t, mux, "bob")
		info := fakeCheckFileInfo(t, mux, token)
		if info.UserCanWrite || !info.ReadOnly {
			t.Errorf("bob: UserCanWrite/ReadOnly = %v/%v, want false/true", info.UserCanWrite, info.ReadOnly)
		}
	})
}

// TestFakeHostModeRefusedOnNonLoopbackPublicURL checks that
// EXCALIDRAW_WOPI_FAKE_HOST=1 alone must not mount the fake host on a
// deployment whose PublicURL is not a loopback address, since the fake
// host holds no auth check of its own.
func TestFakeHostModeRefusedOnNonLoopbackPublicURL(t *testing.T) {
	t.Setenv("EXCALIDRAW_WOPI_FAKE_HOST", "1")

	cfg := testConfig(t) // non-loopback PublicURL
	proofKeys, err := proof.Load(cfg)
	if err != nil {
		t.Fatalf("proof.Load() error = %v", err)
	}
	staticFS := fstest.MapFS{
		"index.html": {Data: []byte(`<script type="application/json" id="ew-config">{}</script>`)},
	}
	srv, err := app.NewServer(cfg, staticFS, proofKeys)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/fakewopi/launch?user=alice", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "access_token") {
		t.Errorf("GET /fakewopi/launch served the fake launch page on a non-loopback PublicURL: %s", rec.Body.String())
	}
}

// fakeLaunch fetches the fake launch page for user and pulls the minted
// access_token and the WOPISrc its form posts to out of the rendered
// HTML, so the test can act as the browser would without a JS runtime.
func fakeLaunch(t *testing.T, mux http.Handler, user string) (token, wopiSrc string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/fakewopi/launch?user="+user, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /fakewopi/launch?user=%s status = %d, body: %s", user, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	token = extractAttr(t, body, `name="access_token" value="`)
	action := extractAttr(t, body, `action="`)
	actionURL, err := url.Parse(action)
	if err != nil {
		t.Fatalf("parse form action %q: %v", action, err)
	}
	wopiSrc = actionURL.Query().Get("WOPISrc")
	if wopiSrc == "" {
		t.Fatalf("form action %q carries no WOPISrc", action)
	}
	return token, wopiSrc
}

func extractAttr(t *testing.T, html, marker string) string {
	t.Helper()
	i := strings.Index(html, marker)
	if i == -1 {
		t.Fatalf("marker %q not found in launch page:\n%s", marker, html)
	}
	rest := html[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j == -1 {
		t.Fatalf("marker %q: unterminated attribute value in launch page:\n%s", marker, html)
	}
	return rest[:j]
}

type fakeFileInfo struct {
	UserCanWrite bool `json:"UserCanWrite"`
	ReadOnly     bool `json:"ReadOnly"`
}

func fakeCheckFileInfo(t *testing.T, mux http.Handler, token string) fakeFileInfo {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/fakewopi/files/f-local?access_token="+token, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CheckFileInfo status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var info fakeFileInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode CheckFileInfo response: %v", err)
	}
	return info
}

type fakeStats struct {
	Size     int64  `json:"size"`
	Version  string `json:"version"`
	PutCount int    `json:"putCount"`
}

func fakeState(t *testing.T, mux http.Handler) fakeStats {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/fakewopi/_state", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/fakewopi/_state status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var stats fakeStats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode /fakewopi/_state response: %v", err)
	}
	return stats
}
