package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// conflictBroadcastHostPort is fixed, not kernel-assigned, for the same
// reason as testhooks_test.go's stubWOPIHostPort: some sandboxes'
// outbound TCP only reaches fixed ports. The room Manager's background
// save loop calls out through wopiclient's http.DefaultClient
// (wopiclient.New(nil, ...) in app.go) against cfg.PublicURL, so this
// server must genuinely listen there, unlike every other test in this
// package that only ever drives srv.Handler in-process.
const conflictBroadcastHostPort = "18101"

// TestConflictAndReloadBroadcastReachAJoinedSocket checks that the app
// wires a real Manager into a real Relay the way NewServer itself does
// (room.WithOnConflictChange / room.WithOnReloadRequired ->
// rel.BroadcastToRoom, internal/app/app.go), and checks a joined socket
// actually receives both broadcasts end to end, rather than only asserting
// that the closures are passed as options. Standing up the full relay is
// not disproportionate here: TestFakeHostMode already does it for every
// other app-level test in this package.
func TestConflictAndReloadBroadcastReachAJoinedSocket(t *testing.T) {
	t.Setenv("EXCALIDRAW_WOPI_FAKE_HOST", "1")

	cfg := testFakeHostConfig(t)
	cfg.PublicURL = "http://127.0.0.1:" + conflictBroadcastHostPort
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

	lis, err := net.Listen("tcp", "127.0.0.1:"+conflictBroadcastHostPort)
	if err != nil {
		t.Fatalf("listen on fixed test port %s: %v", conflictBroadcastHostPort, err)
	}
	realSrv := httptest.NewUnstartedServer(mux)
	realSrv.Listener = lis
	realSrv.Start()
	t.Cleanup(realSrv.Close)
	// Best-effort: RegisterOnShutdown hooks (including the room Manager's
	// own Shutdown flush) run on their own goroutines and are not waited
	// on by srv.Shutdown, so this cannot guarantee the background loop
	// has stopped by the time this test returns; closing the real
	// listener above is what actually makes any straggling call harmless
	// (a plain connection error, not a shared-state race).
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	accessToken, wopiSrc := fakeLaunch(t, mux, "alice")

	// Plant a foreign lock directly at the mounted fake WOPI host, the
	// same way internal/room/host_test.go's TestForeignLockEntersConflict
	// does against its own standalone host: any authenticated token can
	// set an arbitrary lock value (a WOPI lock is a bare string, not tied
	// to one user), so alice's own token is enough to simulate a
	// different editor already holding the file.
	const foreignLock = "some-other-editor-lock"
	lockReq := httptest.NewRequest(http.MethodPost, "/fakewopi/files/f-local?access_token="+accessToken, nil)
	lockReq.Header.Set("X-WOPI-Override", "LOCK")
	lockReq.Header.Set("X-WOPI-Lock", foreignLock)
	lockRec := httptest.NewRecorder()
	mux.ServeHTTP(lockRec, lockReq)
	if lockRec.Code != http.StatusOK {
		t.Fatalf("plant a foreign lock: status = %d, body: %s", lockRec.Code, lockRec.Body.String())
	}

	sessionToken, fileID := realLaunch(t, mux, accessToken, wopiSrc)
	if fileID != "f-local" {
		t.Fatalf("launched fileID = %q, want f-local", fileID)
	}

	c := newWireClient(t, mux)
	c.connect(`{"token":"` + sessionToken + `"}`)
	c.poll() // CONNECT ack
	c.poll() // init-room

	c.emit("join-room", fileID)
	c.poll() // room-user-change
	c.poll() // user-joined
	c.poll() // sync-designate

	// A PutScene wakes the Manager's background loop; it tries to lock
	// the room's file, hits the foreign lock 409, and enters conflict,
	// which app.go's WithOnConflictChange closure turns into a
	// "conflict-state" broadcast to this room.
	putReq := httptest.NewRequest(http.MethodPut, "/api/board", bytes.NewReader([]byte("scene-bytes")))
	putReq.Header.Set("Authorization", "Bearer "+sessionToken)
	putRec := httptest.NewRecorder()
	mux.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusNoContent {
		t.Fatalf("PUT /api/board: status = %d, body: %s", putRec.Code, putRec.Body.String())
	}

	name, payload := c.waitForEvent(t, "conflict-state")
	var conflictState struct {
		InConflict  bool `json:"inConflict"`
		SaveStalled bool `json:"saveStalled"`
	}
	if err := json.Unmarshal(payload, &conflictState); err != nil {
		t.Fatalf("decode %s payload %q: %v", name, payload, err)
	}
	if !conflictState.InConflict {
		t.Fatalf("conflict-state payload = %+v, want inConflict true", conflictState)
	}

	// ResolveConflict(reload) clears the conflict (a second "conflict-state"
	// broadcast) and, since a foreign lock caused it, fires
	// WithOnReloadRequired too: app.go turns that into a "reload-required"
	// broadcast to the same room.
	resolveReq := httptest.NewRequest(http.MethodPost, "/api/board/conflict/resolve", strings.NewReader(`{"overwrite":false}`))
	resolveReq.Header.Set("Authorization", "Bearer "+sessionToken)
	resolveRec := httptest.NewRecorder()
	mux.ServeHTTP(resolveRec, resolveReq)
	if resolveRec.Code != http.StatusNoContent {
		t.Fatalf("POST /api/board/conflict/resolve: status = %d, body: %s", resolveRec.Code, resolveRec.Body.String())
	}

	name, payload = c.waitForEvent(t, "reload-required")
	if name != "reload-required" {
		t.Fatalf("event name = %q, want reload-required; payload=%q", name, payload)
	}
}

// realLaunch drives the real POST /launch flow (the request the fake
// host's auto-submitting form targets) and pulls the minted session token
// and fileId out of the rendered #ew-config script tag.
func realLaunch(t *testing.T, mux http.Handler, accessToken, wopiSrc string) (sessionToken, fileID string) {
	t.Helper()
	ttlMillis := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	form := "access_token=" + accessToken + "&access_token_ttl=" + ttlMillis
	req := httptest.NewRequest(http.MethodPost, "/launch?WOPISrc="+wopiSrc, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /launch: status = %d, body: %s", rec.Code, rec.Body.String())
	}

	const open = `<script type="application/json" id="ew-config">`
	const closeTag = `</script>`
	body := rec.Body.String()
	i := strings.Index(body, open)
	if i == -1 {
		t.Fatalf("launch page missing #ew-config: %s", body)
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, closeTag)
	if j == -1 {
		t.Fatalf("launch page #ew-config missing closing tag: %s", body)
	}

	var cfg struct {
		FileID       string `json:"fileId"`
		SessionToken string `json:"sessionToken"`
	}
	if err := json.Unmarshal([]byte(rest[:j]), &cfg); err != nil {
		t.Fatalf("decode #ew-config %q: %v", rest[:j], err)
	}
	if cfg.SessionToken == "" {
		t.Fatalf("launch page carries no sessionToken: %s", body)
	}
	return cfg.SessionToken, cfg.FileID
}

// wireClient drives the Engine.IO v4 long-polling wire protocol directly
// against an http.Handler, the same in-process technique
// internal/relay's pollingClient uses and for the same reason (this repo's
// sandbox refuses loopback TCP outright): a trimmed-down, package-local
// copy, since that helper lives in relay's own _test.go file and is not
// importable from here.
type wireClient struct {
	t       *testing.T
	h       http.Handler
	sid     string
	pending [][]byte
}

func newWireClient(t *testing.T, h http.Handler) *wireClient {
	t.Helper()
	c := &wireClient{t: t, h: h}
	body := c.nextPacket()
	openBody := c.mustPacket(body, '0')

	var open struct {
		Sid string `json:"sid"`
	}
	if err := json.Unmarshal(openBody, &open); err != nil {
		t.Fatalf("decode engine.io open packet %q: %v", openBody, err)
	}
	c.sid = open.Sid
	return c
}

func (c *wireClient) do(method, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	url := "/socket.io/?EIO=4&transport=polling"
	if c.sid != "" {
		url += "&sid=" + c.sid
	}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, url, reader)
	if body != "" {
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

func (c *wireClient) connect(authPayload string) {
	c.t.Helper()
	rec := c.do(http.MethodPost, "40"+authPayload)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("POST connect: status %d", rec.Code)
	}
}

func (c *wireClient) emit(ev string, args ...any) {
	c.t.Helper()
	packet := append([]any{ev}, args...)
	raw, err := json.Marshal(packet)
	if err != nil {
		c.t.Fatalf("encode emit(%s): %v", ev, err)
	}
	rec := c.do(http.MethodPost, "42"+string(raw))
	if rec.Code != http.StatusOK {
		c.t.Fatalf("POST emit(%s): status %d", ev, rec.Code)
	}
}

func (c *wireClient) poll() (byte, []byte) {
	c.t.Helper()
	return c.mustSocketIOPacket(c.nextPacket())
}

// waitForEvent polls until it sees an EVENT packet named want, ignoring
// (and reporting) anything else, up to a generous deadline: the Manager's
// background loop reacts to a wake() call promptly but asynchronously, so
// the broadcast this is waiting for is not necessarily the very next
// packet emitted.
func (c *wireClient) waitForEvent(t *testing.T, want string) (name string, payload []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pktType, body := c.poll()
		if pktType != '2' {
			continue
		}
		var raw []json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil || len(raw) == 0 {
			continue
		}
		var got string
		_ = json.Unmarshal(raw[0], &got)
		if got != want {
			continue
		}
		if len(raw) > 1 {
			payload = raw[1]
		}
		return got, payload
	}
	t.Fatalf("timed out waiting for a %q event", want)
	return "", nil
}

func (c *wireClient) nextPacket() []byte {
	c.t.Helper()
	if len(c.pending) == 0 {
		rec := c.do(http.MethodGet, "")
		respBody, err := io.ReadAll(rec.Result().Body)
		if err != nil {
			c.t.Fatalf("read response body: %v", err)
		}
		if rec.Code != http.StatusOK || len(respBody) == 0 {
			c.t.Fatalf("engine.io poll: status %d body %q, want status 200 with a body", rec.Code, respBody)
		}
		c.pending = append(c.pending, bytes.Split(respBody, []byte{0x1e})...)
	}
	packet := c.pending[0]
	c.pending = c.pending[1:]
	return packet
}

func (c *wireClient) mustPacket(body []byte, wantType byte) []byte {
	c.t.Helper()
	if len(body) == 0 || body[0] != wantType {
		c.t.Fatalf("engine.io packet = %q, want type %q", body, wantType)
	}
	return body[1:]
}

func (c *wireClient) mustSocketIOPacket(body []byte) (byte, []byte) {
	c.t.Helper()
	msg := c.mustPacket(body, '4')
	if len(msg) == 0 {
		c.t.Fatal("empty socket.io packet")
	}
	return msg[0], msg[1:]
}
