package app

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/wopitest"
)

// envFakeHost turns on the in-process fake WOPI host.
// Read directly with os.Getenv, not through internal/config: that package
// may still be under active change from other work in this repository,
// and a dev-only flag needs no validation or documented default that
// would earn it a place there.
const envFakeHost = "EXCALIDRAW_WOPI_FAKE_HOST"

const (
	fakeHostBasePath = "/fakewopi/files"
	fakeHostFileID   = "f-local"
	fakeHostTokenTTL = 10 * time.Hour
)

// fakeHostAllowed reports whether NewServer should mount the fake WOPI
// host, for a local Playwright suite and manual dev runs that need no
// Drive, no docker, and no signed proof. It also refuses to enable the
// fake host, logging why, when publicURL is not a loopback address: the
// fake host holds no auth check of its own, so serving it from anywhere
// reachable off the local machine would be a real vulnerability, not a
// dev convenience.
func fakeHostAllowed(publicURL string) bool {
	if os.Getenv(envFakeHost) != "1" {
		return false
	}
	if !isLoopbackHost(publicURL) {
		slog.Error(envFakeHost+"=1 refused: PublicURL is not a loopback host; "+
			"the fake WOPI host has no auth check of its own and must never be reachable off this machine",
			"publicURL", publicURL)
		return false
	}
	return true
}

// isLoopbackHost reports whether rawURL's host is "localhost" or an IP
// literal in the loopback range.
func isLoopbackHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newFakeHost builds the fake WOPI host's fixed dev-mode content: one
// writer (alice), one read-only user (bob), and one file that starts
// empty, so the first save exercises the empty-file PutFile rule (an
// unlocked PutFile succeeds on a zero-byte file).
func newFakeHost() *wopitest.Host {
	host := wopitest.New(fakeHostBasePath, hostadapter.LockTTL)
	host.AddUser(wopitest.User{ID: "alice", Name: "Alice", CanWrite: true})
	host.AddUser(wopitest.User{ID: "bob", Name: "Bob", CanWrite: false})
	host.AddFile(fakeHostFileID, fakeHostFileID+".excalidraw", "alice", nil)
	return host
}

// mountFakeHost wires the fake host's WOPI routes plus the two dev-only
// endpoints onto mux: the launch page a developer opens by hand, and the
// state introspection endpoint a fast local test polls instead of
// guessing a sleep.
func mountFakeHost(mux *http.ServeMux, host *wopitest.Host, publicURL string) {
	mux.Handle(fakeHostBasePath+"/", host.Handler())
	mux.HandleFunc("GET /fakewopi/launch", handleFakeLaunch(host, publicURL))
	mux.HandleFunc("GET /fakewopi/_state", handleFakeState(host))
}

// handleFakeLaunch serves the auto-submitting form a WOPI host normally
// posts from its iframe: a WOPISrc query parameter plus access_token and
// access_token_ttl form fields, POSTed straight to /launch.
func handleFakeLaunch(host *wopitest.Host, publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user")
		if userID != "alice" && userID != "bob" {
			http.Error(w, "wopitest: ?user= must be alice or bob", http.StatusBadRequest)
			return
		}

		token := host.MintToken(userID, fakeHostFileID)
		ttlMillis := time.Now().Add(fakeHostTokenTTL).UnixMilli()
		wopiSrc := publicURL + fakeHostBasePath + "/" + fakeHostFileID
		action := "/launch?" + url.Values{"WOPISrc": {wopiSrc}}.Encode()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = fakeLaunchTemplate.Execute(w, fakeLaunchPage{
			Action:      action,
			AccessToken: token,
			TTLMillis:   ttlMillis,
		})
	}
}

type fakeLaunchPage struct {
	Action      string
	AccessToken string
	TTLMillis   int64
}

// fakeLaunchTemplate auto-submits on load, exactly like the hidden iframe
// form the WOPI host posts in production; html/template escapes every
// field, so this stays safe even though every value here is server-minted.
var fakeLaunchTemplate = template.Must(template.New("fake-launch").Parse(`<!DOCTYPE html>
<html><head><title>fake WOPI launch</title></head>
<body>
<form id="f" method="POST" action="{{.Action}}">
<input type="hidden" name="access_token" value="{{.AccessToken}}">
<input type="hidden" name="access_token_ttl" value="{{.TTLMillis}}">
</form>
<script>document.getElementById('f').submit()</script>
</body></html>
`))

// handleFakeState reports the fake file's live size, version, and
// PutFile call count, so a test can poll for a save landing instead of
// sleeping past the client's REST save cadence.
func handleFakeState(host *wopitest.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats, ok := host.Stats(fakeHostFileID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	}
}

// logFakeHostWarning logs once, loudly, that fake-host mode is running.
func logFakeHostWarning() {
	slog.Warn("EXCALIDRAW_WOPI_FAKE_HOST=1: the in-process fake WOPI host is mounted at /fakewopi/; " +
		"this skips the real WOPI host, proof verification, and every production auth check — development only, never enable this in production")
}
