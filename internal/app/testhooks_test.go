package app_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/app"
	"github.com/zeylos/excalidraw-wopi/internal/proof"
)

// stubWOPIHostPort is fixed, not kernel-assigned: in a sandbox whose
// outbound TCP only reaches fixed ports (see e2e/README.md's
// host-networking note), httptest.NewServer's default random port fails
// to dial.
const stubWOPIHostPort = "18099"

// newStubWOPIHost starts an httptest.Server bound to stubWOPIHostPort
// instead of a random one, for the same reason as the constant above.
func newStubWOPIHost(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:"+stubWOPIHostPort)
	if err != nil {
		t.Fatalf("listen on fixed test port %s: %v", stubWOPIHostPort, err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = lis
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// TestTestHooksModeDisabledByDefault checks that NewServer never sets
// testHooks in the launch config unless EXCALIDRAW_WOPI_TEST_HOOKS=1: a
// production deployment must never expose window.__excaTest by accident.
func TestTestHooksModeDisabledByDefault(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodPost, "/launch?WOPISrc=http://wopi.test/f1", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "testHooks") {
		t.Errorf("launch page mentions testHooks while EXCALIDRAW_WOPI_TEST_HOOKS is unset: %s", rec.Body.String())
	}
}

// TestTestHooksModeEnabled covers the dockerized-Drive smoke suite's own
// knob: EXCALIDRAW_WOPI_TEST_HOOKS=1 sets testHooks: true on a launch,
// independent of --fake-host, against a
// stub WOPI host that answers CheckFileInfo unconditionally (the launch
// handler's own proof signing does not require the stub to verify
// anything back).
func TestTestHooksModeEnabled(t *testing.T) {
	t.Setenv("EXCALIDRAW_WOPI_TEST_HOOKS", "1")

	wopiHost := newStubWOPIHost(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"UserId":"u1","UserCanWrite":true}`))
	})

	cfg := testConfig(t)
	cfg.AllowedWOPIOrigins = []string{wopiHost.URL}
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

	form := "access_token=tok&access_token_ttl=" + strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	req := httptest.NewRequest(http.MethodPost, "/launch?WOPISrc="+wopiHost.URL+"/wopi/files/f1", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"testHooks":true`) {
		t.Errorf("launch page does not set testHooks:true with EXCALIDRAW_WOPI_TEST_HOOKS=1: %s", rec.Body.String())
	}
}
