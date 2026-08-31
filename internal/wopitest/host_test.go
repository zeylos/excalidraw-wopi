package wopitest_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
	"github.com/zeylos/excalidraw-wopi/internal/wopitest"
)

// md5Hex is the S3-style ETag Host computes for content: the hex MD5
// digest of the bytes.
func md5Hex(content string) string {
	sum := md5.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

const (
	basePath = "/wopi/files"
	origin   = "http://fakehost.invalid"
)

// localTransport runs a request straight through handler, in the calling
// goroutine, with no socket involved: an httptest.ResponseRecorder
// stands in for the network. wopiclient.Client only ever sees a plain
// http.Client, so this keeps the tests exercising the real client and
// the real Host.Handler() wiring without needing a live listener.
type localTransport struct{ handler http.Handler }

func (t localTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// fixture wires a Host and a real wopiclient.Client against it, through
// localTransport, the same pairing internal/app builds for --fake-host
// dev mode. Testing through the client, rather than hitting Host's HTTP
// routes directly, proves the thing this package exists for: that
// internal/wopiclient plus internal/hostadapter.Drive works against Host
// unchanged.
type fixture struct {
	host   *wopitest.Host
	client *wopiclient.Client
}

func newFixture(t *testing.T, lockTTL time.Duration) fixture {
	t.Helper()
	host := wopitest.New(basePath, lockTTL)
	host.AddUser(wopitest.User{ID: "alice", Name: "Alice", CanWrite: true})
	host.AddUser(wopitest.User{ID: "bob", Name: "Bob", CanWrite: false})
	host.AddFile("f-empty", "empty.excalidraw", "alice", nil)
	host.AddFile("f-seeded", "seeded.excalidraw", "alice", []byte("hello"))

	httpClient := &http.Client{Transport: localTransport{handler: host.Handler()}}
	client := wopiclient.New(httpClient, nil, hostadapter.NewDrive())
	return fixture{host: host, client: client}
}

func (f fixture) src(id string) string {
	return origin + basePath + "/" + id
}

func TestCheckFileInfoFields(t *testing.T) {
	f := newFixture(t, time.Minute)
	token := f.host.MintToken("alice", "f-seeded")

	info, err := f.client.CheckFileInfo(context.Background(), f.src("f-seeded"), token)
	if err != nil {
		t.Fatalf("CheckFileInfo: %v", err)
	}
	if info.BaseFileName != "seeded.excalidraw" {
		t.Errorf("BaseFileName = %q, want %q", info.BaseFileName, "seeded.excalidraw")
	}
	if info.UserID != "alice" || info.UserFriendlyName != "Alice" {
		t.Errorf("UserID/UserFriendlyName = %q/%q, want alice/Alice", info.UserID, info.UserFriendlyName)
	}
	if !info.UserCanWrite || info.ReadOnly {
		t.Errorf("UserCanWrite/ReadOnly = %v/%v, want true/false for a writer", info.UserCanWrite, info.ReadOnly)
	}
	if info.Size != 5 {
		t.Errorf("Size = %d, want 5", info.Size)
	}
	// Version is an S3-style ETag, a content hash rather than a counter:
	// "hello"'s MD5 digest, before any PutFile.
	if want := md5Hex("hello"); info.Version != want {
		t.Errorf("Version = %q, want %q (md5 of the seeded content)", info.Version, want)
	}

	readToken := f.host.MintToken("bob", "f-seeded")
	roInfo, err := f.client.CheckFileInfo(context.Background(), f.src("f-seeded"), readToken)
	if err != nil {
		t.Fatalf("CheckFileInfo (read-only user): %v", err)
	}
	if roInfo.UserCanWrite || !roInfo.ReadOnly {
		t.Errorf("UserCanWrite/ReadOnly = %v/%v, want false/true for a reader", roInfo.UserCanWrite, roInfo.ReadOnly)
	}
}

func TestGetFileReturnsContentAndVersion(t *testing.T) {
	f := newFixture(t, time.Minute)
	token := f.host.MintToken("alice", "f-seeded")

	body, version, err := f.client.GetFile(context.Background(), f.src("f-seeded"), token)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read GetFile body: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("GetFile body = %q, want %q", data, "hello")
	}
	if want := md5Hex("hello"); version != want {
		t.Errorf("GetFile X-WOPI-ItemVersion = %q, want %q", version, want)
	}
}

func TestEmptyFileRule(t *testing.T) {
	t.Run("UnlockedPutFileOnEmptyFileSucceeds", func(t *testing.T) {
		f := newFixture(t, time.Minute)
		token := f.host.MintToken("alice", "f-empty")

		version, err := f.client.PutFile(context.Background(), f.src("f-empty"), token, "", []byte("first save"))
		if err != nil {
			t.Fatalf("PutFile: %v", err)
		}
		if want := md5Hex("first save"); version != want {
			t.Errorf("PutFile version = %q, want %q (md5 of the saved content)", version, want)
		}

		stats, ok := f.host.Stats("f-empty")
		if !ok || stats.PutCount != 1 || stats.Size != int64(len("first save")) {
			t.Errorf("Stats = %+v, ok=%v; want PutCount=1, Size=%d", stats, ok, len("first save"))
		}
	})

	t.Run("UnlockedPutFileOnNonEmptyFileConflicts", func(t *testing.T) {
		f := newFixture(t, time.Minute)
		token := f.host.MintToken("alice", "f-seeded")

		_, err := f.client.PutFile(context.Background(), f.src("f-seeded"), token, "", []byte("should not land"))
		conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err)
		if !ok {
			t.Fatalf("PutFile error = %v, want ErrLockConflict", err)
		}
		if conflict.CurrentLock != "" {
			t.Errorf("CurrentLock = %q, want empty", conflict.CurrentLock)
		}

		stats, _ := f.host.Stats("f-seeded")
		if stats.PutCount != 0 {
			t.Errorf("PutCount = %d, want 0: the conflicting call must not have landed", stats.PutCount)
		}
	})
}

func TestVersionBumpsPerWrite(t *testing.T) {
	f := newFixture(t, time.Minute)
	token := f.host.MintToken("alice", "f-empty")
	ctx := context.Background()

	v1, err := f.client.PutFile(ctx, f.src("f-empty"), token, "", []byte("a"))
	if err != nil {
		t.Fatalf("PutFile 1: %v", err)
	}
	if err := f.client.Lock(ctx, f.src("f-empty"), token, "L1"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	v2, err := f.client.PutFile(ctx, f.src("f-empty"), token, "L1", []byte("ab"))
	if err != nil {
		t.Fatalf("PutFile 2: %v", err)
	}
	if v1 == v2 {
		t.Errorf("version did not move across two PutFile calls: still %q", v2)
	}
}

// TestIdenticalRePutKeepsTheSameVersion checks that Version is an
// S3-style ETag, a content hash, so re-putting the exact same bytes must
// not bump it, unlike a counter would.
func TestIdenticalRePutKeepsTheSameVersion(t *testing.T) {
	f := newFixture(t, time.Minute)
	token := f.host.MintToken("alice", "f-empty")
	ctx := context.Background()

	v1, err := f.client.PutFile(ctx, f.src("f-empty"), token, "", []byte("same content"))
	if err != nil {
		t.Fatalf("PutFile 1: %v", err)
	}
	if err := f.client.Lock(ctx, f.src("f-empty"), token, "L1"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	v2, err := f.client.PutFile(ctx, f.src("f-empty"), token, "L1", []byte("same content"))
	if err != nil {
		t.Fatalf("PutFile 2: %v", err)
	}
	if v1 != v2 {
		t.Errorf("version changed across two identical PutFile calls: %q then %q, want equal", v1, v2)
	}

	stats, ok := f.host.Stats("f-empty")
	if !ok || stats.PutCount != 2 {
		t.Errorf("Stats = %+v, ok=%v; want PutCount=2: an unchanged version must not skip the call itself landing", stats, ok)
	}
}

// TestAuthorizationBearerHeaderAccepted checks that an access token can
// also travel as an Authorization: Bearer header, not only the
// WOPI-standard access_token query parameter.
func TestAuthorizationBearerHeaderAccepted(t *testing.T) {
	f := newFixture(t, time.Minute)
	token := f.host.MintToken("alice", "f-seeded")

	req := httptest.NewRequest(http.MethodGet, f.src("f-seeded"), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.host.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestLockStateMachine(t *testing.T) {
	f := newFixture(t, time.Minute)
	token := f.host.MintToken("alice", "f-empty")
	src := f.src("f-empty")
	ctx := context.Background()

	if err := f.client.Lock(ctx, src, token, "L1"); err != nil {
		t.Fatalf("Lock(L1): %v", err)
	}

	// Same-value LOCK is a refresh, not a conflict.
	if err := f.client.Lock(ctx, src, token, "L1"); err != nil {
		t.Fatalf("Lock(L1) refresh: %v", err)
	}

	if err := f.client.Lock(ctx, src, token, "L2"); !errorsAsConflict(t, err, "L1") {
		t.Fatalf("Lock(L2) while L1 held: %v", err)
	}

	lock, err := f.client.GetLock(ctx, src, token)
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if lock != "L1" {
		t.Errorf("GetLock = %q, want L1", lock)
	}

	if err := f.client.RefreshLock(ctx, src, token, "WRONG"); !errorsAsConflict(t, err, "L1") {
		t.Fatalf("RefreshLock(WRONG): %v", err)
	}
	if err := f.client.RefreshLock(ctx, src, token, "L1"); err != nil {
		t.Fatalf("RefreshLock(L1): %v", err)
	}

	if err := f.client.UnlockAndRelock(ctx, src, token, "L2", "L1"); err != nil {
		t.Fatalf("UnlockAndRelock(L2, old L1): %v", err)
	}
	lock, err = f.client.GetLock(ctx, src, token)
	if err != nil {
		t.Fatalf("GetLock after relock: %v", err)
	}
	if lock != "L2" {
		t.Errorf("GetLock after relock = %q, want L2", lock)
	}

	if err := f.client.Unlock(ctx, src, token, "WRONG"); !errorsAsConflict(t, err, "L2") {
		t.Fatalf("Unlock(WRONG): %v", err)
	}
	if err := f.client.Unlock(ctx, src, token, "L2"); err != nil {
		t.Fatalf("Unlock(L2): %v", err)
	}

	lock, err = f.client.GetLock(ctx, src, token)
	if err != nil {
		t.Fatalf("GetLock after unlock: %v", err)
	}
	if lock != "" {
		t.Errorf("GetLock after unlock = %q, want empty", lock)
	}
}

func TestLockExpiryNeedsRelockNotRefresh(t *testing.T) {
	f := newFixture(t, 20*time.Millisecond)
	token := f.host.MintToken("alice", "f-empty")
	src := f.src("f-empty")
	ctx := context.Background()

	if err := f.client.Lock(ctx, src, token, "L1"); err != nil {
		t.Fatalf("Lock(L1): %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Matching Drive's own cache.touch, which cannot revive an expired
	// lock: a refresh against an expired lock must fail, and GetLock must
	// report no lock held.
	if err := f.client.RefreshLock(ctx, src, token, "L1"); !errorsAsConflict(t, err, "") {
		t.Fatalf("RefreshLock(L1) past expiry: %v", err)
	}

	lock, err := f.client.GetLock(ctx, src, token)
	if err != nil {
		t.Fatalf("GetLock past expiry: %v", err)
	}
	if lock != "" {
		t.Errorf("GetLock past expiry = %q, want empty", lock)
	}

	// A fresh LOCK, not a refresh, must succeed once the old one expired.
	if err := f.client.Lock(ctx, src, token, "L2"); err != nil {
		t.Fatalf("Lock(L2) after expiry: %v", err)
	}
}

func TestBadTokenRejectedOnEveryOp(t *testing.T) {
	f := newFixture(t, time.Minute)
	src := f.src("f-seeded")
	ctx := context.Background()
	const badToken = "not-a-real-token"

	checks := map[string]func() error{
		"CheckFileInfo": func() error {
			_, err := f.client.CheckFileInfo(ctx, src, badToken)
			return err
		},
		"GetFile": func() error {
			_, _, err := f.client.GetFile(ctx, src, badToken)
			return err
		},
		"PutFile": func() error {
			_, err := f.client.PutFile(ctx, src, badToken, "", []byte("x"))
			return err
		},
		"Lock": func() error { return f.client.Lock(ctx, src, badToken, "L1") },
		"GetLock": func() error {
			_, err := f.client.GetLock(ctx, src, badToken)
			return err
		},
	}

	for op, call := range checks {
		t.Run(op, func(t *testing.T) {
			err := call()
			if _, ok := errors.AsType[wopiclient.ErrTokenRejected](err); !ok {
				t.Fatalf("%s with a bad token: %v, want ErrTokenRejected", op, err)
			}
		})
	}
}

func TestReadOnlyTokenRejectedOnWriteOps(t *testing.T) {
	f := newFixture(t, time.Minute)
	src := f.src("f-seeded")
	ctx := context.Background()
	token := f.host.MintToken("bob", "f-seeded")

	checks := map[string]func() error{
		"PutFile": func() error {
			_, err := f.client.PutFile(ctx, src, token, "", []byte("x"))
			return err
		},
		"Lock":        func() error { return f.client.Lock(ctx, src, token, "L1") },
		"RefreshLock": func() error { return f.client.RefreshLock(ctx, src, token, "L1") },
		"Unlock":      func() error { return f.client.Unlock(ctx, src, token, "L1") },
		"GetLock": func() error {
			_, err := f.client.GetLock(ctx, src, token)
			return err
		},
	}

	for op, call := range checks {
		t.Run(op, func(t *testing.T) {
			err := call()
			if _, ok := errors.AsType[wopiclient.ErrNoWriteAccess](err); !ok {
				t.Fatalf("%s with a read-only token: %v, want ErrNoWriteAccess", op, err)
			}
		})
	}

	// A read-only user can still read.
	if _, err := f.client.CheckFileInfo(ctx, src, token); err != nil {
		t.Errorf("CheckFileInfo with a read-only token: %v, want no error", err)
	}
	if body, _, err := f.client.GetFile(ctx, src, token); err != nil {
		t.Errorf("GetFile with a read-only token: %v, want no error", err)
	} else {
		body.Close()
	}
}

func TestPutFileRequiresExactOverrideHeader(t *testing.T) {
	f := newFixture(t, time.Minute)
	token := f.host.MintToken("alice", "f-empty")

	req := httptest.NewRequest(http.MethodPost, f.src("f-empty")+"/contents?access_token="+token, strings.NewReader("x"))
	// Deliberately no X-WOPI-Override header: Drive's PutFile route needs
	// it set to exactly "PUT".
	rec := httptest.NewRecorder()
	f.host.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func errorsAsConflict(t *testing.T, err error, wantLock string) bool {
	t.Helper()
	conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err)
	if !ok {
		return false
	}
	if conflict.CurrentLock != wantLock {
		t.Errorf("ErrLockConflict.CurrentLock = %q, want %q", conflict.CurrentLock, wantLock)
	}
	return true
}
