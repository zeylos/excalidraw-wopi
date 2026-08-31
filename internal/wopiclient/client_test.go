// External test package: it imports hostadapter to exercise Client against
// the real Drive profile, and hostadapter imports wopiclient, so this test
// file cannot live in package wopiclient itself.
package wopiclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

// fakeSigner returns predictable proof values, so a test can assert the
// exact headers a Client sends. It also records the url argument, so a
// test can assert it matches the request the fake WOPI host actually saw.
type fakeSigner struct {
	calls  int
	gotURL string
}

func (f *fakeSigner) Sign(accessToken, url string) (proof, proofOld, timestamp string) {
	f.calls++
	f.gotURL = url
	return "proof-" + accessToken, "proofold-" + accessToken, "1234567890"
}

// newHTTPClient starts handler on a Unix domain socket, not a loopback
// TCP port: some sandboxes that run this suite block outbound TCP to
// 127.0.0.1 but allow Unix sockets, and a Unix socket serves this test
// equally well on a host with no such restriction.
func newHTTPClient(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "wopi.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })

	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

func newClient(t *testing.T, handler http.HandlerFunc, signer wopiclient.RequestSigner) (*wopiclient.Client, string) {
	t.Helper()
	httpClient := newHTTPClient(t, handler)
	return wopiclient.New(httpClient, signer, hostadapter.NewDrive()), "http://wopi.test/api/v1.0/wopi/files/abc"
}

func TestCheckFileInfoSuccess(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Query().Get("access_token") != "tok" {
			t.Errorf("access_token query = %q, want tok", r.URL.Query().Get("access_token"))
		}
		info := wopiclient.FileInfo{BaseFileName: "board.excalidraw", UserID: "u1", Version: "etag1", UserCanWrite: true}
		body, _ := json.Marshal(info)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	c, src := newClient(t, handler, nil)

	info, err := c.CheckFileInfo(context.Background(), src, "tok")
	if err != nil {
		t.Fatalf("CheckFileInfo() error = %v", err)
	}
	if info.BaseFileName != "board.excalidraw" || info.Version != "etag1" || !info.UserCanWrite {
		t.Errorf("CheckFileInfo() = %+v, unexpected fields", info)
	}
}

func TestSignedRequestShape(t *testing.T) {
	signer := &fakeSigner{}
	var gotProof, gotProofOld, gotTimestamp, gotOverride string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotProof = r.Header.Get("X-WOPI-Proof")
		gotProofOld = r.Header.Get("X-WOPI-ProofOld")
		gotTimestamp = r.Header.Get("X-WOPI-TimeStamp")
		gotOverride = r.Header.Get("X-WOPI-Override")

		wantURL := "http://" + r.Host + r.URL.RequestURI()
		if signer.gotURL != wantURL {
			t.Errorf("signer received url = %q, want %q", signer.gotURL, wantURL)
		}
		w.WriteHeader(http.StatusOK)
	}
	c, src := newClient(t, handler, signer)

	if err := c.Lock(context.Background(), src, "tok", "lock1"); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if gotProof != "proof-tok" {
		t.Errorf("X-WOPI-Proof = %q, want proof-tok", gotProof)
	}
	if gotProofOld != "proofold-tok" {
		t.Errorf("X-WOPI-ProofOld = %q, want proofold-tok", gotProofOld)
	}
	if gotTimestamp != "1234567890" {
		t.Errorf("X-WOPI-TimeStamp = %q, want 1234567890", gotTimestamp)
	}
	if gotOverride != "LOCK" {
		t.Errorf("X-WOPI-Override = %q, want LOCK", gotOverride)
	}
}

func TestUnsignedRequestOmitsProofHeaders(t *testing.T) {
	var sawProof bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		sawProof = r.Header.Get("X-WOPI-Proof") != "" || r.Header.Get("X-WOPI-TimeStamp") != ""
		w.WriteHeader(http.StatusOK)
	}
	c, src := newClient(t, handler, nil)

	if err := c.Unlock(context.Background(), src, "tok", "lock1"); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	if sawProof {
		t.Error("request carried proof headers with a nil signer")
	}
}

func TestGetFileSuccess(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/contents") {
			t.Errorf("path = %s, want suffix /contents", r.URL.Path)
		}
		w.Header().Set("X-WOPI-ItemVersion", "etag-42")
		_, _ = w.Write([]byte("scene bytes"))
	}
	c, src := newClient(t, handler, nil)

	rc, version, err := c.GetFile(context.Background(), src, "tok")
	if err != nil {
		t.Fatalf("GetFile() error = %v", err)
	}
	defer rc.Close()

	body, _ := io.ReadAll(rc)
	if string(body) != "scene bytes" {
		t.Errorf("body = %q, want %q", body, "scene bytes")
	}
	if version != "etag-42" {
		t.Errorf("version = %q, want etag-42", version)
	}
}

func TestGetFileSendsMaxExpectedSizeHeader(t *testing.T) {
	var got string
	handler := func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-WOPI-MaxExpectedSize")
		_, _ = w.Write([]byte("x"))
	}
	c, src := newClient(t, handler, nil)

	if _, _, err := c.GetFile(context.Background(), src, "tok", 1024); err != nil {
		t.Fatalf("GetFile() error = %v", err)
	}
	if got != "1024" {
		t.Errorf("X-WOPI-MaxExpectedSize = %q, want 1024", got)
	}
}

func TestGetFileOmitsMaxExpectedSizeHeaderByDefault(t *testing.T) {
	var saw bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		saw = r.Header.Get("X-WOPI-MaxExpectedSize") != ""
		_, _ = w.Write([]byte("x"))
	}
	c, src := newClient(t, handler, nil)

	if _, _, err := c.GetFile(context.Background(), src, "tok"); err != nil {
		t.Fatalf("GetFile() error = %v", err)
	}
	if saw {
		t.Error("GetFile without a max size must not send X-WOPI-MaxExpectedSize")
	}
}

// TestGetFileContentsURLPreservesExistingQuery checks that a WOPISrc that
// already carries a query string must survive contents URL building.
// String concatenation would append "/contents" after the query, so the
// request would miss the /contents path entirely.
func TestGetFileContentsURLPreservesExistingQuery(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/contents") {
			t.Errorf("path = %s, want suffix /contents", r.URL.Path)
		}
		if got := r.URL.Query().Get("lang"); got != "en" {
			t.Errorf("query lang = %q, want en", got)
		}
		_, _ = w.Write([]byte("x"))
	}
	httpClient := newHTTPClient(t, handler)
	c := wopiclient.New(httpClient, nil, hostadapter.NewDrive())

	if _, _, err := c.GetFile(context.Background(), "http://wopi.test/api/v1.0/wopi/files/abc?lang=en", "tok"); err != nil {
		t.Fatalf("GetFile() error = %v", err)
	}
}

func TestGetFileMaxSizeExceeded(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
	}
	c, src := newClient(t, handler, nil)

	_, _, err := c.GetFile(context.Background(), src, "tok")
	if _, ok := errors.AsType[wopiclient.ErrTooLarge](err); !ok {
		t.Errorf("GetFile() error = %v, want ErrTooLarge", err)
	}
}

func TestPutFileSuccess(t *testing.T) {
	var gotOverride, gotLock, gotContentType string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotOverride = r.Header.Get("X-WOPI-Override")
		gotLock = r.Header.Get("X-WOPI-Lock")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Errorf("request body = %q, want payload", body)
		}
		w.Header().Set("X-WOPI-ItemVersion", "etag-99")
		w.WriteHeader(http.StatusOK)
	}
	c, src := newClient(t, handler, nil)

	version, err := c.PutFile(context.Background(), src, "tok", "lock1", []byte("payload"))
	if err != nil {
		t.Fatalf("PutFile() error = %v", err)
	}
	if version != "etag-99" {
		t.Errorf("version = %q, want etag-99", version)
	}
	if gotOverride != "PUT" {
		t.Errorf("X-WOPI-Override = %q, want PUT", gotOverride)
	}
	if gotLock != "lock1" {
		t.Errorf("X-WOPI-Lock = %q, want lock1", gotLock)
	}
	if gotContentType != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", gotContentType)
	}
}

func TestPutFileEmptyLockOmitsHeader(t *testing.T) {
	var sawLockHeader bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, sawLockHeader = r.Header["X-Wopi-Lock"]
		w.WriteHeader(http.StatusOK)
	}
	c, src := newClient(t, handler, nil)

	if _, err := c.PutFile(context.Background(), src, "tok", "", []byte("x")); err != nil {
		t.Fatalf("PutFile() error = %v", err)
	}
	if sawLockHeader {
		t.Error("PutFile sent an X-WOPI-Lock header for an empty lock")
	}
}

// driveEmptyFilePutFile mimics Drive's rule (wopi/viewsets.py
// _put_file_content): an unlocked PutFile succeeds only when the stored
// file is still empty; otherwise it is a 409 with an empty X-WOPI-Lock.
func driveEmptyFilePutFile(t *testing.T, initialSize int) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	size := initialSize
	currentLock := ""
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		lock := r.Header.Get("X-WOPI-Lock")
		if lock == "" {
			if size > 0 {
				w.Header().Set("X-WOPI-Lock", "")
				w.WriteHeader(http.StatusConflict)
				return
			}
		} else if lock != currentLock {
			w.Header().Set("X-WOPI-Lock", currentLock)
			w.WriteHeader(http.StatusConflict)
			return
		}

		body, _ := io.ReadAll(r.Body)
		size = len(body)
		w.Header().Set("X-WOPI-ItemVersion", "etag-put")
		w.WriteHeader(http.StatusOK)
	}
}

func TestPutFileEmptyFileRuleAllowsFirstSave(t *testing.T) {
	c, src := newClient(t, driveEmptyFilePutFile(t, 0), nil)

	if _, err := c.PutFile(context.Background(), src, "tok", "", []byte("first save")); err != nil {
		t.Fatalf("PutFile() into an empty file: error = %v", err)
	}
}

func TestPutFileEmptyFileRuleRejectsWhenNotEmpty(t *testing.T) {
	c, src := newClient(t, driveEmptyFilePutFile(t, 100), nil)

	_, err := c.PutFile(context.Background(), src, "tok", "", []byte("no lock"))
	conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err)
	if !ok {
		t.Fatalf("PutFile() error = %v, want ErrLockConflict", err)
	}
	if conflict.CurrentLock != "" {
		t.Errorf("CurrentLock = %q, want empty", conflict.CurrentLock)
	}
}

func TestPutFileTooLarge(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}
	c, src := newClient(t, handler, nil)

	_, err := c.PutFile(context.Background(), src, "tok", "lock1", []byte("x"))
	if _, ok := errors.AsType[wopiclient.ErrTooLarge](err); !ok {
		t.Errorf("PutFile() error = %v, want ErrTooLarge", err)
	}
}

func TestWrongOverrideOnContentsReturns404(t *testing.T) {
	// Drive returns 404 for any POST .../contents whose X-WOPI-Override is
	// not exactly PUT. The client always sends PUT; this asserts that a
	// host's 404 maps to ErrNotFound regardless of cause.
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-WOPI-Override") != "PUT" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
	c, src := newClient(t, handler, nil)

	_, err := c.PutFile(context.Background(), src, "tok", "lock1", []byte("x"))
	if _, ok := errors.AsType[wopiclient.ErrNotFound](err); !ok {
		t.Errorf("PutFile() error = %v, want ErrNotFound", err)
	}
}

func TestLockConflictReturnsCurrentLock(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-WOPI-Lock", "someone-elses-lock")
		w.WriteHeader(http.StatusConflict)
	}
	c, src := newClient(t, handler, nil)

	err := c.Lock(context.Background(), src, "tok", "mine")
	conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err)
	if !ok {
		t.Fatalf("Lock() error = %v, want ErrLockConflict", err)
	}
	if conflict.CurrentLock != "someone-elses-lock" {
		t.Errorf("CurrentLock = %q, want someone-elses-lock", conflict.CurrentLock)
	}
}

func TestLockConflictWithExpiredLockHasEmptyHeader(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-WOPI-Lock", "")
		w.WriteHeader(http.StatusConflict)
	}
	c, src := newClient(t, handler, nil)

	err := c.RefreshLock(context.Background(), src, "tok", "mine")
	conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err)
	if !ok {
		t.Fatalf("RefreshLock() error = %v, want ErrLockConflict", err)
	}
	if conflict.CurrentLock != "" {
		t.Errorf("CurrentLock = %q, want empty", conflict.CurrentLock)
	}
}

func TestGetLockReturnsHeaderValue(t *testing.T) {
	var gotOverride string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotOverride = r.Header.Get("X-WOPI-Override")
		w.Header().Set("X-WOPI-Lock", "current-lock")
		w.WriteHeader(http.StatusOK)
	}
	c, src := newClient(t, handler, nil)

	lock, err := c.GetLock(context.Background(), src, "tok")
	if err != nil {
		t.Fatalf("GetLock() error = %v", err)
	}
	if lock != "current-lock" {
		t.Errorf("GetLock() = %q, want current-lock", lock)
	}
	if gotOverride != "GET_LOCK" {
		t.Errorf("X-WOPI-Override = %q, want GET_LOCK", gotOverride)
	}
}

func TestUnlockAndRelockSendsBothLockHeaders(t *testing.T) {
	var gotLock, gotOldLock, gotOverride string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotLock = r.Header.Get("X-WOPI-Lock")
		gotOldLock = r.Header.Get("X-WOPI-OldLock")
		gotOverride = r.Header.Get("X-WOPI-Override")
		w.WriteHeader(http.StatusOK)
	}
	c, src := newClient(t, handler, nil)

	if err := c.UnlockAndRelock(context.Background(), src, "tok", "new-lock", "old-lock"); err != nil {
		t.Fatalf("UnlockAndRelock() error = %v", err)
	}
	if gotLock != "new-lock" {
		t.Errorf("X-WOPI-Lock = %q, want new-lock", gotLock)
	}
	if gotOldLock != "old-lock" {
		t.Errorf("X-WOPI-OldLock = %q, want old-lock", gotOldLock)
	}
	if gotOverride != "LOCK" {
		t.Errorf("X-WOPI-Override = %q, want LOCK", gotOverride)
	}
}

func TestTokenRejected403(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}
	c, src := newClient(t, handler, nil)

	_, err := c.CheckFileInfo(context.Background(), src, "bad-token")
	if _, ok := errors.AsType[wopiclient.ErrTokenRejected](err); !ok {
		t.Errorf("CheckFileInfo() error = %v, want ErrTokenRejected", err)
	}
}

func TestNoWriteAccess401(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}
	c, src := newClient(t, handler, nil)

	err := c.Lock(context.Background(), src, "read-only-token", "lock1")
	if _, ok := errors.AsType[wopiclient.ErrNoWriteAccess](err); !ok {
		t.Errorf("Lock() error = %v, want ErrNoWriteAccess", err)
	}
}

func TestProofRejected500(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	c, src := newClient(t, handler, &fakeSigner{})

	_, err := c.CheckFileInfo(context.Background(), src, "tok")
	if _, ok := errors.AsType[wopiclient.ErrProofRejected](err); !ok {
		t.Errorf("CheckFileInfo() error = %v, want ErrProofRejected", err)
	}
}

func TestGenericHTTPErrorFallback(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("odd status"))
	}
	c, src := newClient(t, handler, nil)

	_, err := c.CheckFileInfo(context.Background(), src, "tok")
	httpErr, ok := errors.AsType[wopiclient.HTTPError](err)
	if !ok {
		t.Fatalf("CheckFileInfo() error = %v, want HTTPError", err)
	}
	if httpErr.Status != http.StatusTeapot || httpErr.Op != wopiclient.OpCheckFileInfo {
		t.Errorf("HTTPError = %+v, unexpected fields", httpErr)
	}
}

// TestTransportErrorRedactsAccessToken checks that a dial failure wraps
// net/http's *url.Error, which carries the full request URL
// (access_token query value included); that value must never reach the
// returned error's message, since callers log it.
func TestTransportErrorRedactsAccessToken(t *testing.T) {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", filepath.Join(t.TempDir(), "does-not-exist.sock"))
			},
		},
	}
	c := wopiclient.New(httpClient, nil, hostadapter.NewDrive())

	const token = "super-secret-access-token-value"
	_, err := c.CheckFileInfo(context.Background(), "http://wopi.test/api/v1.0/wopi/files/abc", token)
	if err == nil {
		t.Fatal("CheckFileInfo() over a broken dial: want an error, got nil")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaks the access token: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("error does not show a REDACTED marker: %v", err)
	}
}
