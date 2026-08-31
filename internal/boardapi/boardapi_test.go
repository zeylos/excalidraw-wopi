package boardapi_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/boardapi"
	"github.com/zeylos/excalidraw-wopi/internal/session"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

const testSecret = "a test secret with enough entropy"

func testSessions(t *testing.T) *session.Manager {
	t.Helper()
	m, err := session.New([]byte(testSecret))
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	return m
}

func mintToken(t *testing.T, m *session.Manager, p session.MintParams) string {
	t.Helper()
	if p.ExpiresAt.IsZero() {
		p.ExpiresAt = time.Now().Add(time.Hour)
	}
	raw, err := m.Mint(p)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	return raw
}

// stubGetFile is a GetFileer that returns a fixed body or a fixed error,
// and records the src/token/maxExpectedSize it was called with.
type stubGetFile struct {
	body       string
	err        error
	gotSrc     string
	gotTok     string
	gotMaxSize int64
	nCalled    int
}

func (s *stubGetFile) GetFile(_ context.Context, src, token string, maxExpectedSize ...int64) (io.ReadCloser, string, error) {
	s.nCalled++
	s.gotSrc, s.gotTok = src, token
	if len(maxExpectedSize) > 0 {
		s.gotMaxSize = maxExpectedSize[0]
	}
	if s.err != nil {
		return nil, "", s.err
	}
	return io.NopCloser(strings.NewReader(s.body)), "etag-1", nil
}

// identityWrap stands in for internal/peers' Cluster.Middleware: every
// test here runs a single Handler with no multi-replica routing.
func identityWrap(h http.Handler) http.Handler { return h }

func newRequest(method, target, body, bearer string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func TestGetBoardServesStoreHitWithoutCallingWOPI(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	const wopiSrc = "https://drive.example/files/file-1"
	if err := store.PutScene(wopiSrc, []byte(`{"elements":[]}`), boardapi.SceneMeta{UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("PutScene() error = %v", err)
	}
	getFile := &stubGetFile{}
	h := boardapi.New(sessions, getFile, store, 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-1", WOPISrc: wopiSrc, CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", token))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cto := rec.Header().Get("X-Content-Type-Options"); cto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", cto)
	}
	if rec.Body.String() != `{"elements":[]}` {
		t.Errorf("body = %q", rec.Body.String())
	}
	if getFile.nCalled != 0 {
		t.Errorf("GetFile called %d times, want 0 on a store hit", getFile.nCalled)
	}
}

// TestGetBoardKeysStoreByFullWOPISrcNotBareFileID checks that two WOPI
// hosts (or two tenants) that happen to mint the same trailing file id
// must not collide in RoomStore. A store keyed on the bare file id would
// let a session launched against one WOPISrc read another WOPISrc's
// stored scene.
func TestGetBoardKeysStoreByFullWOPISrcNotBareFileID(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	getFile := &stubGetFile{body: `{"from":"drive"}`}
	h := boardapi.New(sessions, getFile, store, 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	const uuid = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	victimSrc := "https://victim.example/api/v1.0/wopi/files/" + uuid
	attackerSrc := "https://attacker.example/api/v1.0/wopi/files/" + uuid

	if err := store.PutScene(victimSrc, []byte(`{"secret":"victim scene"}`), boardapi.SceneMeta{UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("PutScene() error = %v", err)
	}

	attackerToken := mintToken(t, sessions, session.MintParams{
		FileID: uuid, WOPISrc: attackerSrc, AccessToken: "attacker-tok", CanWrite: true,
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", attackerToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == `{"secret":"victim scene"}` {
		t.Fatal("a session for a different WOPISrc read the victim's stored scene: store keys must be per-WOPISrc, never the bare file id")
	}
	if rec.Body.String() != `{"from":"drive"}` {
		t.Errorf("body = %q, want the WOPI fallback body (a genuine store miss for this WOPISrc)", rec.Body.String())
	}
}

func TestGetBoardFallsBackToWOPIOnStoreMiss(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	getFile := &stubGetFile{body: `{"from":"drive"}`}
	h := boardapi.New(sessions, getFile, store, 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{
		FileID: "file-2", WOPISrc: "https://drive.example/files/file-2", AccessToken: "drive-tok", CanWrite: true,
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", token))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"from":"drive"}` {
		t.Errorf("body = %q", rec.Body.String())
	}
	if getFile.nCalled != 1 {
		t.Errorf("GetFile called %d times, want 1 on a store miss", getFile.nCalled)
	}
	if getFile.gotSrc != "https://drive.example/files/file-2" || getFile.gotTok != "drive-tok" {
		t.Errorf("GetFile called with src=%q token=%q", getFile.gotSrc, getFile.gotTok)
	}
	if getFile.gotMaxSize != 1024 {
		t.Errorf("GetFile called with maxExpectedSize = %d, want 1024", getFile.gotMaxSize)
	}
}

// TestGetBoardUpstreamOversizeSceneReturns502 checks that a GetFile
// fallback body larger than the configured scene limit must not be
// buffered in full and handed to the client; the read is bounded, and an
// overrun is a 502.
func TestGetBoardUpstreamOversizeSceneReturns502(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	getFile := &stubGetFile{body: strings.Repeat("x", 20)}
	h := boardapi.New(sessions, getFile, store, 8)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{
		FileID: "file-9", WOPISrc: "https://drive.example/files/file-9", CanWrite: true,
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", token))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestGetBoardWOPITokenRejectedReturns403(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	getFile := &stubGetFile{err: wopiclient.ErrTokenRejected{}}
	h := boardapi.New(sessions, getFile, store, 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-3", CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", token))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestGetBoardWOPIOtherErrorReturns502(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	getFile := &stubGetFile{err: errors.New("boom")}
	h := boardapi.New(sessions, getFile, store, 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-3", CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", token))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestGetBoardMissingAuthReturns401(t *testing.T) {
	sessions := testSessions(t)
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestGetBoardInvalidTokenReturns401(t *testing.T) {
	sessions := testSessions(t)
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", "not-a-real-jwt"))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestGetBoardExpiredTokenReturns401(t *testing.T) {
	sessions := testSessions(t)
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{
		FileID: "file-4", CanWrite: true, ExpiresAt: time.Now().Add(-time.Minute),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", token))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestPutBoardStoresSceneAndReturns204(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	h := boardapi.New(sessions, &stubGetFile{}, store, 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	const wopiSrc = "https://drive.example/files/file-5"
	token := mintToken(t, sessions, session.MintParams{FileID: "file-5", WOPISrc: wopiSrc, CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPut, "/api/board", `{"elements":[1]}`, token))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	data, ok := store.GetScene(wopiSrc)
	if !ok {
		t.Fatal("PutScene did not persist the scene")
	}
	if string(data) != `{"elements":[1]}` {
		t.Errorf("stored scene = %q", data)
	}
}

func TestPutBoardReadOnlySessionReturns403(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	h := boardapi.New(sessions, &stubGetFile{}, store, 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-6", CanWrite: false})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPut, "/api/board", `{}`, token))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if _, ok := store.GetScene("file-6"); ok {
		t.Error("a rejected PUT must not reach the store")
	}
}

func TestPutBoardOversizeSceneReturns413(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	h := boardapi.New(sessions, &stubGetFile{}, store, 8)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-7", CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPut, "/api/board", "123456789", token))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if _, ok := store.GetScene("file-7"); ok {
		t.Error("an oversize PUT must not reach the store")
	}
}

func TestPutBoardAtExactLimitSucceeds(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	h := boardapi.New(sessions, &stubGetFile{}, store, 8)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-8", CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPut, "/api/board", "12345678", token))

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
}

func TestPutBoardMissingAuthReturns401(t *testing.T) {
	sessions := testSessions(t)
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPut, "/api/board", `{}`, ""))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestMemStoreGetSceneReturnsACopy(t *testing.T) {
	store := newMemStore()
	if err := store.PutScene("f", []byte("original"), boardapi.SceneMeta{}); err != nil {
		t.Fatalf("PutScene() error = %v", err)
	}

	got, ok := store.GetScene("f")
	if !ok {
		t.Fatal("GetScene() ok = false")
	}
	got[0] = 'X'

	got2, _ := store.GetScene("f")
	if string(got2) != "original" {
		t.Errorf("stored scene mutated through the returned slice: %q", got2)
	}
}

func TestMemStoreGetSceneMiss(t *testing.T) {
	store := newMemStore()
	if _, ok := store.GetScene("missing"); ok {
		t.Error("GetScene() on an empty store: ok = true, want false")
	}
}

// stubConflictStore is a ConflictStore test double: conflict maps a
// wopiSrc to its scripted Conflict() answer, and every ResolveConflict
// call is recorded for assertions. resolveErr, when set, makes the next
// ResolveConflict call fail.
type stubConflictStore struct {
	conflict    map[string]bool
	saveStalled map[string]bool
	resolveErr  error
	resolveCall []resolveCall
}

type resolveCall struct {
	wopiSrc   string
	overwrite bool
}

func newStubConflictStore() *stubConflictStore {
	return &stubConflictStore{conflict: map[string]bool{}, saveStalled: map[string]bool{}}
}

func (s *stubConflictStore) Conflict(wopiSrc string) bool {
	return s.conflict[wopiSrc]
}

func (s *stubConflictStore) SaveStalled(wopiSrc string) bool {
	return s.saveStalled[wopiSrc]
}

func (s *stubConflictStore) ResolveConflict(wopiSrc string, overwrite bool) error {
	s.resolveCall = append(s.resolveCall, resolveCall{wopiSrc: wopiSrc, overwrite: overwrite})
	return s.resolveErr
}

func TestGetConflictReportsStoreState(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	const wopiSrc = "https://drive.example/files/file-conflict"
	conflicts := newStubConflictStore()
	conflicts.conflict[wopiSrc] = true
	h := boardapi.New(sessions, &stubGetFile{}, store, 1024, boardapi.WithConflictStore(conflicts))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-conflict", WOPISrc: wopiSrc, CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board/conflict", "", token))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"inConflict":true,"saveStalled":false}` {
		t.Errorf("body = %q, want {\"inConflict\":true,\"saveStalled\":false}", got)
	}
}

func TestGetConflictWithNoConflictStoreConfiguredAnswersFalse(t *testing.T) {
	sessions := testSessions(t)
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-x", CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board/conflict", "", token))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"inConflict":false,"saveStalled":false}` {
		t.Errorf("body = %q, want {\"inConflict\":false,\"saveStalled\":false}", got)
	}
}

func TestGetConflictMissingAuthReturns401(t *testing.T) {
	sessions := testSessions(t)
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board/conflict", "", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestResolveConflictOverwriteCallsStoreWithTrue(t *testing.T) {
	sessions := testSessions(t)
	const wopiSrc = "https://drive.example/files/file-resolve-1"
	conflicts := newStubConflictStore()
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024, boardapi.WithConflictStore(conflicts))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-resolve-1", WOPISrc: wopiSrc, CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPost, "/api/board/conflict/resolve", `{"overwrite":true}`, token))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	if len(conflicts.resolveCall) != 1 || conflicts.resolveCall[0] != (resolveCall{wopiSrc: wopiSrc, overwrite: true}) {
		t.Fatalf("resolveCall = %+v, want one call with wopiSrc %q overwrite=true", conflicts.resolveCall, wopiSrc)
	}
}

func TestResolveConflictReloadCallsStoreWithFalse(t *testing.T) {
	sessions := testSessions(t)
	const wopiSrc = "https://drive.example/files/file-resolve-2"
	conflicts := newStubConflictStore()
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024, boardapi.WithConflictStore(conflicts))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-resolve-2", WOPISrc: wopiSrc, CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPost, "/api/board/conflict/resolve", `{"overwrite":false}`, token))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	if len(conflicts.resolveCall) != 1 || conflicts.resolveCall[0] != (resolveCall{wopiSrc: wopiSrc, overwrite: false}) {
		t.Fatalf("resolveCall = %+v, want one call with wopiSrc %q overwrite=false", conflicts.resolveCall, wopiSrc)
	}
}

func TestResolveConflictReadOnlySessionReturns403(t *testing.T) {
	sessions := testSessions(t)
	conflicts := newStubConflictStore()
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024, boardapi.WithConflictStore(conflicts))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-resolve-3", CanWrite: false})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPost, "/api/board/conflict/resolve", `{"overwrite":true}`, token))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(conflicts.resolveCall) != 0 {
		t.Error("a rejected resolve must not reach the store")
	}
}

func TestResolveConflictMissingAuthReturns401(t *testing.T) {
	sessions := testSessions(t)
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPost, "/api/board/conflict/resolve", `{"overwrite":true}`, ""))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestResolveConflictMalformedBodyReturns400(t *testing.T) {
	sessions := testSessions(t)
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024, boardapi.WithConflictStore(newStubConflictStore()))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-resolve-4", CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPost, "/api/board/conflict/resolve", `not json`, token))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestResolveConflictStoreErrorReturns500(t *testing.T) {
	sessions := testSessions(t)
	conflicts := newStubConflictStore()
	conflicts.resolveErr = errors.New("boom")
	h := boardapi.New(sessions, &stubGetFile{}, newMemStore(), 1024, boardapi.WithConflictStore(conflicts))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{FileID: "file-resolve-5", CanWrite: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPost, "/api/board/conflict/resolve", `{"overwrite":true}`, token))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// stubObserver records every Claims Observe receives.
type stubObserver struct {
	observed []session.Claims
}

func (o *stubObserver) Observe(claims session.Claims) {
	o.observed = append(o.observed, claims)
}

// TestObserverSeesEveryAuthenticatedRequest checks that the configured
// Observer, when set, is notified on both GET and PUT, and not at all on
// a request that never authenticates.
func TestObserverSeesEveryAuthenticatedRequest(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	observer := &stubObserver{}
	h := boardapi.New(sessions, &stubGetFile{body: `{}`}, store, 1024, boardapi.WithObserver(observer))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	const wopiSrc = "https://drive.example/files/file-9"
	token := mintToken(t, sessions, session.MintParams{
		FileID: "file-9", WOPISrc: wopiSrc, UserID: "alice", AccessToken: "tok", CanWrite: true,
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodPut, "/api/board", `{"elements":[]}`, token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(http.MethodGet, "/api/board", "", "not-a-real-token"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d, want 401", rec.Code)
	}

	if len(observer.observed) != 2 {
		t.Fatalf("Observe called %d times, want 2 (one per authenticated request)", len(observer.observed))
	}
	for i, claims := range observer.observed {
		if claims.WOPISrc != wopiSrc || claims.UserID != "alice" {
			t.Errorf("observed[%d] = %+v, want WOPISrc %q and UserID alice", i, claims, wopiSrc)
		}
	}
}

// TestOwnershipCheckRejectsAllFourEndpoints checks the multi-replica
// invariant: a refused fileID gets 421 "wrong replica" on every endpoint,
// and the Observer never runs, since Observe would register the token
// into the room manager on a replica that must not own the room.
func TestOwnershipCheckRejectsAllFourEndpoints(t *testing.T) {
	sessions := testSessions(t)
	store := newMemStore()
	observer := &stubObserver{}
	h := boardapi.New(sessions, &stubGetFile{body: `{}`}, store, 1024,
		boardapi.WithObserver(observer),
		boardapi.WithOwnershipCheck(func(_ string) bool { return false }))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, identityWrap)

	token := mintToken(t, sessions, session.MintParams{
		FileID: "file-not-owned", WOPISrc: "https://drive.example/files/file-not-owned",
		UserID: "alice", AccessToken: "tok", CanWrite: true,
	})

	requests := []*http.Request{
		newRequest(http.MethodGet, "/api/board", "", token),
		newRequest(http.MethodPut, "/api/board", `{"elements":[]}`, token),
		newRequest(http.MethodGet, "/api/board/conflict", "", token),
		newRequest(http.MethodPost, "/api/board/conflict/resolve", `{"overwrite":true}`, token),
	}
	for _, req := range requests {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("%s %s: status = %d, want %d", req.Method, req.URL.Path, rec.Code, http.StatusMisdirectedRequest)
		}
		if !strings.Contains(rec.Body.String(), "wrong replica") {
			t.Errorf("%s %s: body = %q, want it to mention %q", req.Method, req.URL.Path, rec.Body.String(), "wrong replica")
		}
	}

	if len(observer.observed) != 0 {
		t.Errorf("Observe called %d times, want 0: a non-owner replica must never register the token", len(observer.observed))
	}
}
