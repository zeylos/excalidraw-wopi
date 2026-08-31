// Package wopitest implements an in-memory WOPI host for tests and for
// the service's own --fake-host dev mode. It mimics La Suite Drive's
// behavior closely enough that internal/wopiclient, paired with
// internal/hostadapter.Drive, works against it unchanged: the
// same status-code quirks (403 for a bad token on every op, 401 for a
// read-only token on a write op), the same lock state machine, and the
// same empty-file PutFile rule.
//
// Host verifies no WOPI proof signature: it is a nil-signer host, meant
// for a real signer to call unchecked (a fake host has no discovery XML
// for Drive to trust a key from in the first place).
package wopitest

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

const (
	headerOverride    = "X-WOPI-Override"
	headerLock        = "X-WOPI-Lock"
	headerOldLock     = "X-WOPI-OldLock"
	headerContentType = "Content-Type"
	itemVersionHeader = "X-WOPI-ItemVersion"
)

// User is one WOPI login the host recognizes.
type User struct {
	ID       string
	Name     string
	CanWrite bool
}

// FileStats reports a file's current state, for a dev-mode introspection
// endpoint that a test polls instead of guessing a sleep.
type FileStats struct {
	Size     int64  `json:"size"`
	Version  string `json:"version"`
	PutCount int    `json:"putCount"`
}

// file holds one item's mutable state. lock is the empty string when the
// host holds no lock; lockExpiry is meaningless while lock is empty.
// version is an S3-style ETag: a content hash, not a counter, so an
// identical re-put keeps the same version instead of always bumping it.
type file struct {
	name     string
	ownerID  string
	content  []byte
	version  string
	lock     string
	lockExp  time.Time
	putCount int
}

// contentVersion computes content's S3-style ETag: the hex MD5 digest of
// its bytes.
func contentVersion(content []byte) string {
	sum := md5.Sum(content)
	return hex.EncodeToString(sum[:])
}

// tokenClaim is what MintToken bound an opaque token to.
type tokenClaim struct {
	userID string
	fileID string
}

// Host is an in-memory WOPI host. The zero value is not usable; build one
// with New.
type Host struct {
	mu       sync.Mutex
	basePath string
	lockTTL  time.Duration
	users    map[string]User
	files    map[string]*file
	tokens   map[string]tokenClaim
}

// New builds a Host. basePath is the URL path prefix under which
// Handler's routes live, e.g. "/wopi/files"; a caller mounts Handler()
// at basePath+"/" on its own mux. lockTTL is the lifetime a LOCK or a
// refresh assigns; tests pass a short value to exercise expiry without a
// real 30-minute wait (Drive's own TTL is 30 min, held as
// hostadapter.LockTTL).
func New(basePath string, lockTTL time.Duration) *Host {
	return &Host{
		basePath: strings.TrimSuffix(basePath, "/"),
		lockTTL:  lockTTL,
		users:    make(map[string]User),
		files:    make(map[string]*file),
		tokens:   make(map[string]tokenClaim),
	}
}

// AddUser registers a login the host accepts a token for.
func (h *Host) AddUser(u User) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.users[u.ID] = u
}

// AddFile registers an item. content is copied; a nil or empty content
// starts the item at size 0, so a caller can exercise the empty-file
// PutFile rule from the first save.
func (h *Host) AddFile(id, name, ownerID string, content []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := append([]byte(nil), content...)
	h.files[id] = &file{
		name:    name,
		ownerID: ownerID,
		content: cp,
		version: contentVersion(cp),
	}
}

// MintToken issues a fresh opaque token bound to (userID, fileID),
// mimicking a WOPI host minting a per-user, per-file access token at
// launch. The caller is responsible for userID and
// fileID naming a registered User and file; an unknown pair simply never
// matches on later use, which the fake-host quirks then reject the same
// way an invalid token is rejected.
func (h *Host) MintToken(userID, fileID string) string {
	token := randomToken()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokens[token] = tokenClaim{userID: userID, fileID: fileID}
	return token
}

// Stats reports fileID's current size, version, and PutFile call count.
// It returns ok=false when no such file is registered.
func (h *Host) Stats(fileID string) (FileStats, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.files[fileID]
	if !ok {
		return FileStats{}, false
	}
	return FileStats{
		Size:     int64(len(f.content)),
		Version:  f.version,
		PutCount: f.putCount,
	}, true
}

// Lock reports fileID's current effective lock value ("" when the host
// holds no lock) and whether fileID is registered. An HA test polls this
// to prove a surviving replica re-locked a file after the owning replica
// died.
func (h *Host) Lock(fileID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.files[fileID]
	if !ok {
		return "", false
	}
	return f.effectiveLock(time.Now()), true
}

// Content returns a copy of fileID's currently stored bytes and whether
// fileID is registered. An HA test polls this to prove a save landed.
func (h *Host) Content(fileID string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.files[fileID]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), f.content...), true
}

// Handler returns the http.Handler that serves every WOPI route this
// host implements, rooted at basePath. A caller mounts it directly on a
// mux, e.g. mux.Handle(basePath+"/", host.Handler()); it expects the
// request's URL path to still carry basePath (nothing strips it).
func (h *Host) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+h.basePath+"/{id}", h.handleCheckFileInfo)
	mux.HandleFunc("GET "+h.basePath+"/{id}/contents", h.handleGetFile)
	mux.HandleFunc("POST "+h.basePath+"/{id}/contents", h.handlePutFile)
	mux.HandleFunc("POST "+h.basePath+"/{id}", h.handleFileOp)
	return mux
}

func (h *Host) handleCheckFileInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	h.mu.Lock()
	defer h.mu.Unlock()

	user, f, ok := h.authenticateLocked(w, r, id, false)
	if !ok {
		return
	}

	info := wopiclient.FileInfo{
		BaseFileName:     f.name,
		OwnerID:          f.ownerID,
		UserID:           user.ID,
		UserFriendlyName: user.Name,
		Size:             int64(len(f.content)),
		Version:          f.version,
		UserCanWrite:     user.CanWrite,
		ReadOnly:         !user.CanWrite,
		SupportsLocks:    true,
		SupportsGetLock:  true,
		SupportsUpdate:   true,
	}

	w.Header().Set(headerContentType, "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(info)
}

func (h *Host) handleGetFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	h.mu.Lock()
	_, f, ok := h.authenticateLocked(w, r, id, false)
	if !ok {
		h.mu.Unlock()
		return
	}
	content := append([]byte(nil), f.content...)
	version := f.version
	h.mu.Unlock()

	w.Header().Set(itemVersionHeader, version)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// handlePutFile implements PutFile, including the empty-file rule: an
// unlocked call (no X-WOPI-Lock header) succeeds only while the host
// holds no lock and the stored file is still 0 bytes. A locked call must
// carry exactly the lock value the host holds.
func (h *Host) handlePutFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// WOPI requires the override header set to exactly "PUT" on this
	// route. Answering 404, not 400, to a wrong or missing value is the
	// Drive quirk this host mimics.
	if r.Header.Get(headerOverride) != "PUT" {
		http.NotFound(w, r)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	_, f, ok := h.authenticateLocked(w, r, id, true)
	if !ok {
		return
	}

	lock := r.Header.Get(headerLock)
	current := f.effectiveLock(time.Now())

	if lock == "" {
		if current != "" || len(f.content) != 0 {
			writeLockConflict(w, current)
			return
		}
	} else if lock != current {
		writeLockConflict(w, current)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "wopitest: read request body", http.StatusInternalServerError)
		return
	}
	f.content = body
	// An S3-style ETag: a content hash, not a counter, so an identical
	// re-put keeps the same version instead of always bumping it.
	f.version = contentVersion(body)
	f.putCount++

	w.Header().Set(itemVersionHeader, f.version)
	w.WriteHeader(http.StatusOK)
}

// handleFileOp implements every lock operation a WOPI client sends as a
// POST against the bare item URL, dispatched on X-WOPI-Override. GET_LOCK
// needs write ability just like the other four, so one write-gated
// authenticateLocked call covers the whole handler.
func (h *Host) handleFileOp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	override := r.Header.Get(headerOverride)

	h.mu.Lock()
	defer h.mu.Unlock()

	_, f, ok := h.authenticateLocked(w, r, id, true)
	if !ok {
		return
	}

	switch override {
	case "LOCK":
		if oldLock := r.Header.Get(headerOldLock); oldLock != "" {
			h.unlockAndRelock(w, f, r.Header.Get(headerLock), oldLock)
		} else {
			h.lock(w, f, r.Header.Get(headerLock))
		}
	case "GET_LOCK":
		h.getLock(w, f)
	case "REFRESH_LOCK":
		h.refreshLock(w, f, r.Header.Get(headerLock))
	case "UNLOCK":
		h.unlock(w, f, r.Header.Get(headerLock))
	default:
		http.NotFound(w, r)
	}
}

// lock acquires newLock, or refreshes it when the host already holds
// that exact value (a same-value LOCK is a refresh).
func (h *Host) lock(w http.ResponseWriter, f *file, newLock string) {
	now := time.Now()
	current := f.effectiveLock(now)

	if current != "" && current != newLock {
		writeLockConflict(w, current)
		return
	}
	f.lock = newLock
	f.lockExp = now.Add(h.lockTTL)
	w.WriteHeader(http.StatusOK)
}

func (h *Host) getLock(w http.ResponseWriter, f *file) {
	w.Header().Set(headerLock, f.effectiveLock(time.Now()))
	w.WriteHeader(http.StatusOK)
}

// refreshLock extends the TTL of a lock the host already holds under
// exactly this value. An expired or absent lock cannot be refreshed;
// the caller must LOCK instead.
func (h *Host) refreshLock(w http.ResponseWriter, f *file, lock string) {
	now := time.Now()
	current := f.effectiveLock(now)
	if current == "" || current != lock {
		writeLockConflict(w, current)
		return
	}
	f.lockExp = now.Add(h.lockTTL)
	w.WriteHeader(http.StatusOK)
}

func (h *Host) unlock(w http.ResponseWriter, f *file, lock string) {
	now := time.Now()
	current := f.effectiveLock(now)
	if current == "" || current != lock {
		writeLockConflict(w, current)
		return
	}
	f.lock = ""
	w.WriteHeader(http.StatusOK)
}

// unlockAndRelock implements UNLOCK_AND_RELOCK: a LOCK call carrying
// X-WOPI-OldLock. It requires the host to hold exactly oldLock, then
// swaps it for newLock in one step.
func (h *Host) unlockAndRelock(w http.ResponseWriter, f *file, newLock, oldLock string) {
	now := time.Now()
	current := f.effectiveLock(now)
	if current == "" || current != oldLock {
		writeLockConflict(w, current)
		return
	}
	f.lock = newLock
	f.lockExp = now.Add(h.lockTTL)
	w.WriteHeader(http.StatusOK)
}

// effectiveLock returns the lock value f currently carries, clearing and
// reporting "" once now is past its TTL: an expired lock behaves as no
// lock at all, matching Drive's own lock cache, whose cache.touch
// cannot revive an expired lock either.
func (f *file) effectiveLock(now time.Time) string {
	if f.lock != "" && now.After(f.lockExp) {
		f.lock = ""
	}
	return f.lock
}

func writeLockConflict(w http.ResponseWriter, currentLock string) {
	w.Header().Set(headerLock, currentLock)
	w.WriteHeader(http.StatusConflict)
}

// authenticateLocked validates the request's access token for fileID and
// writes a Drive-shaped error response itself on rejection: 403 for a
// token that is missing, unknown, or bound to a different file (Drive
// downgrades every bad-token case to 403 rather than the spec's 401, as
// reflected in hostadapter.Drive.MapError), and 401 when needsWrite is
// set and the resolved user cannot write (GET_LOCK needs write access
// too, the same as PutFile and the other lock operations). Callers must
// hold h.mu.
func (h *Host) authenticateLocked(w http.ResponseWriter, r *http.Request, fileID string, needsWrite bool) (User, *file, bool) {
	token := requestToken(r)
	claim, ok := h.tokens[token]
	if !ok || token == "" || claim.fileID != fileID {
		http.Error(w, "wopitest: access token rejected", http.StatusForbidden)
		return User{}, nil, false
	}

	user, userOK := h.users[claim.userID]
	f, fileOK := h.files[fileID]
	if !userOK || !fileOK {
		http.Error(w, "wopitest: access token rejected", http.StatusForbidden)
		return User{}, nil, false
	}

	if needsWrite && !user.CanWrite {
		http.Error(w, "wopitest: token has no write access", http.StatusUnauthorized)
		return User{}, nil, false
	}

	return user, f, true
}

// requestToken reads the access token from r: the query parameter WOPI
// callers use, or, as a fallback, an "Authorization: Bearer" header, for
// a caller that prefers to keep the token out of the URL.
func requestToken(r *http.Request) string {
	if token := r.URL.Query().Get("access_token"); token != "" {
		return token
	}
	const prefix = "Bearer "
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, prefix) {
		return strings.TrimPrefix(auth, prefix)
	}
	return ""
}

func randomToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read on the standard reader does not fail in
		// practice; a panic here would only ever fire on a broken host.
		panic("wopitest: read random token bytes: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
