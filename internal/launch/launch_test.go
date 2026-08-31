package launch_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/launch"
	"github.com/zeylos/excalidraw-wopi/internal/session"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

const indexHTML = `<!doctype html>
<html>
<body>
<script type="application/json" id="ew-config">{}</script>
</body>
</html>
`

// testMaxImageBytes stands in for config.Config.MaxImageBytes across this
// file's tests; TestLaunchInjectsMaxImageBytes checks the actual injection
// with a value distinct from config's own default, so a copy-paste of the
// default would not mask a broken wire-through.
const testMaxImageBytes int64 = 12345678

// newWOPIClient starts handler on a Unix domain socket, following the
// pattern in internal/wopiclient/client_test.go: some sandboxes block
// outbound loopback TCP but allow Unix sockets.
func newWOPIClient(t *testing.T, handler http.HandlerFunc) *wopiclient.Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "wopi.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
	return wopiclient.New(httpClient, nil, hostadapter.NewDrive())
}

func testSessions(t *testing.T) *session.Manager {
	t.Helper()
	m, err := session.New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	return m
}

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte(indexHTML)},
	}
}

func launchRequest(wopiSrc, accessToken string, ttl time.Time) *http.Request {
	form := url.Values{
		"access_token":     {accessToken},
		"access_token_ttl": {strconv.FormatInt(ttl.UnixMilli(), 10)},
	}
	target := "/launch?WOPISrc=" + url.QueryEscape(wopiSrc) + "&lang=en&closebutton=1"
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func extractConfig(t *testing.T, body string) map[string]any {
	t.Helper()
	const open = `<script type="application/json" id="ew-config">`
	const closeTag = `</script>`
	start := strings.Index(body, open)
	if start == -1 {
		t.Fatalf("body has no ew-config script tag: %s", body)
	}
	start += len(open)
	end := strings.Index(body[start:], closeTag)
	if end == -1 {
		t.Fatalf("ew-config script tag has no closing tag: %s", body)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(body[start:start+end]), &cfg); err != nil {
		t.Fatalf("ew-config blob is not valid JSON: %v\nblob: %s", err, body[start:start+end])
	}
	return cfg
}

func TestLaunchSuccess(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("access_token") != "tok" {
			t.Errorf("access_token = %q, want tok", r.URL.Query().Get("access_token"))
		}
		info := wopiclient.FileInfo{
			BaseFileName:     "board.excalidraw",
			UserID:           "u1",
			UserFriendlyName: "Ada Lovelace",
			UserCanWrite:     true,
			Version:          "etag1",
		}
		body, _ := json.Marshal(info)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	client := newWOPIClient(t, handler)
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	wopiSrc := "http://wopi.test/api/v1.0/wopi/files/file-42"
	// The JWT exp claim is a whole-second NumericDate, so the round trip
	// through Mint/Verify loses sub-second precision; truncate to match.
	expiresAt := time.Now().Add(10 * time.Hour).Truncate(time.Second)
	req := launchRequest(wopiSrc, "tok", expiresAt)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "frame-ancestors http://wopi.test" {
		t.Errorf("Content-Security-Policy = %q, want %q", csp, "frame-ancestors http://wopi.test")
	}

	cfg := extractConfig(t, rec.Body.String())
	if cfg["fileId"] != "file-42" {
		t.Errorf("fileId = %v, want file-42", cfg["fileId"])
	}
	if cfg["fileName"] != "board.excalidraw" {
		t.Errorf("fileName = %v", cfg["fileName"])
	}
	if cfg["userId"] != "u1" {
		t.Errorf("userId = %v", cfg["userId"])
	}
	if cfg["userName"] != "Ada Lovelace" {
		t.Errorf("userName = %v", cfg["userName"])
	}
	if cfg["canWrite"] != true {
		t.Errorf("canWrite = %v, want true", cfg["canWrite"])
	}
	if cfg["apiBase"] != "/api" {
		t.Errorf("apiBase = %v, want /api", cfg["apiBase"])
	}
	if cfg["socketPath"] != "/socket.io" {
		t.Errorf("socketPath = %v, want /socket.io", cfg["socketPath"])
	}
	if cfg["maxImageBytes"] != float64(testMaxImageBytes) {
		t.Errorf("maxImageBytes = %v, want %d", cfg["maxImageBytes"], testMaxImageBytes)
	}

	sessionToken, _ := cfg["sessionToken"].(string)
	if sessionToken == "" {
		t.Fatal("sessionToken is empty")
	}

	claims, err := testSessionsVerify(t, sessionToken)
	if err != nil {
		t.Fatalf("session verify of the minted token: %v", err)
	}
	if claims.FileID != "file-42" || claims.WOPISrc != wopiSrc || claims.AccessToken != "tok" {
		t.Errorf("claims = %+v, unexpected fields", claims)
	}
	if !claims.ExpiresAt.Equal(expiresAt) {
		t.Errorf("claims.ExpiresAt = %v, want %v", claims.ExpiresAt, expiresAt)
	}
}

// testSessionsVerify rebuilds a Manager with the same fixed test secret
// testSessions uses, so the test can verify a token minted by a handler
// built from a separate Manager instance.
func testSessionsVerify(t *testing.T, raw string) (session.Claims, error) {
	t.Helper()
	m, err := session.New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	return m.Verify(raw)
}

// TestLaunchInjectsMaxImageBytes checks that the value New() is built
// with reaches the frontend's config blob unchanged, so an operator's
// EXCALIDRAW_WOPI_MAX_IMAGE_BYTES setting actually takes effect
// client-side.
func TestLaunchInjectsMaxImageBytes(t *testing.T) {
	client := newWOPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		info := wopiclient.FileInfo{UserID: "u1", UserCanWrite: true}
		body, _ := json.Marshal(info)
		_, _ = w.Write(body)
	})

	const wantBytes int64 = 4 * 1024 * 1024
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, wantBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	cfg := extractConfig(t, rec.Body.String())
	if cfg["maxImageBytes"] != float64(wantBytes) {
		t.Errorf("maxImageBytes = %v, want %d", cfg["maxImageBytes"], wantBytes)
	}
}

// TestLaunchTestHooksOnlyWhenOptedIn checks that the normal launch path
// emits no testHooks field at all, and only a Handler built with
// WithTestHooks sets it to true.
func TestLaunchTestHooksOnlyWhenOptedIn(t *testing.T) {
	newHandler := func(t *testing.T, opts ...launch.Option) *launch.Handler {
		t.Helper()
		client := newWOPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
			info := wopiclient.FileInfo{UserID: "u1", UserCanWrite: true}
			body, _ := json.Marshal(info)
			_, _ = w.Write(body)
		})
		return launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes, opts...)
	}
	doLaunch := func(t *testing.T, h *launch.Handler) map[string]any {
		t.Helper()
		req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Now().Add(time.Hour))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		return extractConfig(t, rec.Body.String())
	}

	t.Run("default", func(t *testing.T) {
		cfg := doLaunch(t, newHandler(t))
		if _, present := cfg["testHooks"]; present {
			t.Errorf("cfg[testHooks] = %v, want the field absent entirely", cfg["testHooks"])
		}
	})

	t.Run("WithTestHooks", func(t *testing.T) {
		cfg := doLaunch(t, newHandler(t, launch.WithTestHooks()))
		if cfg["testHooks"] != true {
			t.Errorf("cfg[testHooks] = %v, want true", cfg["testHooks"])
		}
	})
}

func TestLaunchEscapesFileNameAgainstScriptInjection(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		info := wopiclient.FileInfo{
			BaseFileName: `nasty</script><script>alert(1)</script>.excalidraw`,
			UserID:       "u1",
			UserCanWrite: true,
		}
		body, _ := json.Marshal(info)
		_, _ = w.Write(body)
	}
	client := newWOPIClient(t, handler)
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Count(body, "<script") != 1 {
		t.Errorf("body has %d <script tags, want exactly 1 (the ew-config tag); a fileName escaped a tag boundary:\n%s",
			strings.Count(body, "<script"), body)
	}
	cfg := extractConfig(t, body)
	if cfg["fileName"] != `nasty</script><script>alert(1)</script>.excalidraw` {
		t.Errorf("fileName round-trip = %v", cfg["fileName"])
	}
}

func TestLaunchTokenRejectedReturns403(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}
	client := newWOPIClient(t, handler)
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "bad-token", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestLaunchProofRejectedReturns502(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	client := newWOPIClient(t, handler)
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestLaunchOtherWOPIFailureReturns502(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}
	client := newWOPIClient(t, handler)
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestLaunchMissingWOPISrcReturns400(t *testing.T) {
	client := newWOPIClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	form := url.Values{"access_token": {"tok"}, "access_token_ttl": {"1234567890000"}}
	req := httptest.NewRequest(http.MethodPost, "/launch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestLaunchMissingAccessTokenReturns400(t *testing.T) {
	client := newWOPIClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestLaunchRelativeWOPISrcReturns400(t *testing.T) {
	client := newWOPIClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("/api/v1.0/wopi/files/f1", "tok", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestLaunchDisallowedOriginReturns403 checks the origin allowlist that
// blocks SSRF: a WOPISrc whose origin is not in the configured allowlist
// must never reach CheckFileInfo, and must never get a minted session.
func TestLaunchDisallowedOriginReturns403(t *testing.T) {
	var called bool
	client := newWOPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("http://attacker.example/api/v1.0/wopi/files/f1", "tok", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Error("a disallowed WOPISrc must never reach CheckFileInfo")
	}
}

// TestLaunchEmptyAllowlistRejectsEveryRequest checks that, with no
// configured origins, /launch refuses every request, even one whose
// WOPISrc would otherwise look fine.
func TestLaunchEmptyAllowlistRejectsEveryRequest(t *testing.T) {
	client := newWOPIClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := launch.New(client, testSessions(t), testFS(), nil, testMaxImageBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestLaunchPastAccessTokenTTLReturns400(t *testing.T) {
	client := newWOPIClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Now().Add(-time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestLaunchZeroAccessTokenTTLReturns400(t *testing.T) {
	client := newWOPIClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Unix(0, 0))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestLaunchHugeAccessTokenTTLIsClamped checks the clamp: a session must
// never outlive maxSessionTTL, however far in the future the host's
// access_token_ttl claims to reach.
func TestLaunchHugeAccessTokenTTLIsClamped(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		info := wopiclient.FileInfo{BaseFileName: "b.excalidraw", UserID: "u1", UserCanWrite: true}
		body, _ := json.Marshal(info)
		_, _ = w.Write(body)
	}
	client := newWOPIClient(t, handler)
	h := launch.New(client, testSessions(t), testFS(), []string{"http://wopi.test"}, testMaxImageBytes)

	before := time.Now()
	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Now().Add(1000*time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	cfg := extractConfig(t, rec.Body.String())
	sessionToken, _ := cfg["sessionToken"].(string)
	claims, err := testSessionsVerify(t, sessionToken)
	if err != nil {
		t.Fatalf("verify minted token: %v", err)
	}

	maxAllowed := before.Add(10 * time.Hour).Add(time.Minute) // slack for test wall-clock drift
	if claims.ExpiresAt.After(maxAllowed) {
		t.Errorf("ExpiresAt = %v, want clamped to about %v", claims.ExpiresAt, before.Add(10*time.Hour))
	}
}

// TestRenderPageAgainstBuiltIndexHTML guards against a real vite build
// reordering the #ew-config script tag's attributes, which would break
// renderPage's literal placeholder match even though every other test
// here uses a hand-written fixture. It skips when the frontend has not
// been built.
func TestRenderPageAgainstBuiltIndexHTML(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "dist", "index.html"))
	if err != nil {
		t.Skipf("web/dist/index.html not built, skipping: %v", err)
	}
	fsys := fstest.MapFS{"index.html": {Data: data}}

	handler := func(w http.ResponseWriter, _ *http.Request) {
		info := wopiclient.FileInfo{BaseFileName: "b.excalidraw", UserID: "u1", UserCanWrite: true}
		body, _ := json.Marshal(info)
		_, _ = w.Write(body)
	}
	client := newWOPIClient(t, handler)
	h := launch.New(client, testSessions(t), fsys, []string{"http://wopi.test"}, testMaxImageBytes)

	req := launchRequest("http://wopi.test/api/v1.0/wopi/files/f1", "tok", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	extractConfig(t, rec.Body.String()) // fails the test itself if the ew-config tag is missing
}
