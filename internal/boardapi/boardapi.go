// Package boardapi implements the frontend-facing REST API: GET and PUT
// /api/board. Every request authenticates with the session JWT.
package boardapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/session"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

// SceneMeta carries bookkeeping about a stored scene. UserID names the
// posting session, so internal/room's save loop can prefer that
// session's token without RoomStore carrying the token itself.
type SceneMeta struct {
	UpdatedAt time.Time
	UserID    string
}

// RoomStore holds the last posted scene per WOPISrc. The key is the
// full WOPISrc, not the bare file id: two WOPI hosts can hand out the
// same file id, and the id alone would mix their scenes. Production
// wires internal/room's Manager here, which also runs the WOPI save
// loop.
type RoomStore interface {
	PutScene(wopiSrc string, data []byte, meta SceneMeta) error
	GetScene(wopiSrc string) ([]byte, bool)
}

// GetFileer is the subset of wopiclient.Client the Handler calls to
// fall back to the WOPI host when RoomStore holds no scene for a file.
// The variadic maxExpectedSize keeps the interface stable for callers
// without the hint; when given, it is sent as X-WOPI-MaxExpectedSize.
type GetFileer interface {
	GetFile(ctx context.Context, src, token string, maxExpectedSize ...int64) (io.ReadCloser, string, error)
}

// Observer receives the claims from every authenticated board API
// request. internal/room's Manager implements it to keep its per-room
// token registry current. It stays separate from RoomStore so a plain
// scene store needs no Observe method; the Handler treats a nil
// Observer as "nothing to notify".
type Observer interface {
	Observe(claims session.Claims)
}

// ConflictStore reports and resolves a room's conflict state.
// internal/room's Manager implements it. With a nil ConflictStore, GET
// /api/board/conflict always answers {"inConflict": false} and resolve
// is a no-op 204.
type ConflictStore interface {
	Conflict(wopiSrc string) bool
	// SaveStalled reports a dirty room whose save attempts keep
	// failing. It is distinct from Conflict: a stalled save can come
	// from a host or network fault rather than an outside edit.
	SaveStalled(wopiSrc string) bool
	ResolveConflict(wopiSrc string, overwrite bool) error
}

// Option configures optional Handler behavior.
type Option func(*Handler)

// WithObserver sets the Observer the Handler notifies on every
// authenticated request.
func WithObserver(o Observer) Option {
	return func(h *Handler) { h.observer = o }
}

// WithConflictStore sets the store behind the conflict endpoints.
func WithConflictStore(cs ConflictStore) Option {
	return func(h *Handler) { h.conflicts = cs }
}

// WithOwnershipCheck sets the function authenticate uses to reject a
// request for a file this replica does not own (internal/peers). A nil
// check treats every replica as the owner of every file.
func WithOwnershipCheck(isOwner func(fileID string) bool) Option {
	return func(h *Handler) { h.isOwner = isOwner }
}

// Handler serves the board REST API.
type Handler struct {
	sessions      *session.Manager
	client        GetFileer
	store         RoomStore
	maxSceneBytes int64
	observer      Observer
	conflicts     ConflictStore
	isOwner       func(fileID string) bool
}

// New builds a board API Handler. sessions verifies the bearer JWT;
// client fetches the scene from the WOPI host on a store miss; store
// holds the last scene posted per file; maxSceneBytes caps a body
// (cfg.MaxSceneBytes).
func New(sessions *session.Manager, client GetFileer, store RoomStore, maxSceneBytes int64, opts ...Option) *Handler {
	h := &Handler{sessions: sessions, client: client, store: store, maxSceneBytes: maxSceneBytes}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// RegisterRoutes wires the board API routes onto mux, each wrapped
// with wrap (internal/peers' Cluster.Middleware in production).
func (h *Handler) RegisterRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /api/board", wrap(http.HandlerFunc(h.handleGetBoard)))
	mux.Handle("PUT /api/board", wrap(http.HandlerFunc(h.handlePutBoard)))
	mux.Handle("GET /api/board/conflict", wrap(http.HandlerFunc(h.handleGetConflict)))
	mux.Handle("POST /api/board/conflict/resolve", wrap(http.HandlerFunc(h.handleResolveConflict)))
}

// handleGetBoard serves the last scene RoomStore holds, or, on a store
// miss, a live WOPI GetFile.
func (h *Handler) handleGetBoard(w http.ResponseWriter, r *http.Request) {
	claims, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}

	if data, ok := h.store.GetScene(claims.WOPISrc); ok {
		writeJSON(w, data)
		return
	}

	rc, _, err := h.client.GetFile(r.Context(), claims.WOPISrc, claims.AccessToken, h.maxSceneBytes)
	if err != nil {
		writeWOPIError(w, err)
		return
	}
	defer rc.Close()

	// Read one byte past the limit, so a body that lands exactly on it
	// is distinguishable from one that exceeds it.
	data, err := io.ReadAll(io.LimitReader(rc, h.maxSceneBytes+1))
	if err != nil {
		slog.Error("boardapi: read GetFile body", "error", err)
		http.Error(w, "upstream WOPI host error", http.StatusBadGateway)
		return
	}
	if int64(len(data)) > h.maxSceneBytes {
		http.Error(w, "upstream scene exceeds the configured size limit", http.StatusBadGateway)
		return
	}
	writeJSON(w, data)
}

// handlePutBoard stores the posted scene into RoomStore. It does not
// call WOPI PutFile: internal/room runs the save loop.
func (h *Handler) handlePutBoard(w http.ResponseWriter, r *http.Request) {
	claims, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if !claims.CanWrite {
		http.Error(w, "read-only session cannot save", http.StatusForbidden)
		return
	}

	// Read one byte past the limit, so a body that lands exactly on it
	// is distinguishable from one that exceeds it.
	data, err := io.ReadAll(io.LimitReader(r.Body, h.maxSceneBytes+1))
	if err != nil {
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	if int64(len(data)) > h.maxSceneBytes {
		http.Error(w, "scene exceeds the configured size limit", http.StatusRequestEntityTooLarge)
		return
	}

	if err := h.store.PutScene(claims.WOPISrc, data, SceneMeta{UpdatedAt: time.Now(), UserID: claims.UserID}); err != nil {
		slog.Error("boardapi: store scene", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// conflictStateResponse is the wire shape of GET /api/board/conflict
// and of internal/relay's conflict-state push.
type conflictStateResponse struct {
	InConflict  bool `json:"inConflict"`
	SaveStalled bool `json:"saveStalled"`
}

// handleGetConflict answers whether the caller's room is currently paused
// on a detected conflict, and whether its save loop is stalled.
func (h *Handler) handleGetConflict(w http.ResponseWriter, r *http.Request) {
	claims, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}

	var inConflict, saveStalled bool
	if h.conflicts != nil {
		inConflict = h.conflicts.Conflict(claims.WOPISrc)
		saveStalled = h.conflicts.SaveStalled(claims.WOPISrc)
	}
	writeJSONValue(w, conflictStateResponse{InConflict: inConflict, SaveStalled: saveStalled})
}

// resolveConflictRequest is the POST /api/board/conflict/resolve body.
type resolveConflictRequest struct {
	Overwrite bool `json:"overwrite"`
}

// handleResolveConflict resolves the room's conflict: overwrite=true
// forces the retained scene to the WOPI host; overwrite=false drops it
// so the next GET proxies fresh host content. Only a writer may
// resolve: a read-only session that chose the reload branch would take
// the overwrite option away from every writer.
func (h *Handler) handleResolveConflict(w http.ResponseWriter, r *http.Request) {
	claims, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if !claims.CanWrite {
		http.Error(w, "read-only session cannot resolve a conflict", http.StatusForbidden)
		return
	}

	var body resolveConflictRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	if h.conflicts != nil {
		if err := h.conflicts.ResolveConflict(claims.WOPISrc, body.Overwrite); err != nil {
			slog.Error("boardapi: resolve conflict", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// errWrongReplica marks a request whose verified token names a file
// this replica does not own; the untrusted "room" hint routed it here.
// Callers answer 421, not 401, so the client retries against the
// re-resolved owner.
var errWrongReplica = errors.New("boardapi: this replica does not own the requested file")

// authenticate reads and verifies the bearer session JWT, checks
// ownership, then notifies the Observer. The ownership check must run
// before Observe: Observe registers the token into the room manager,
// and a non-owner replica would create a second, orphaned room. It
// never logs or echoes back the raw header value.
func (h *Handler) authenticate(r *http.Request) (session.Claims, error) {
	const prefix = "Bearer "
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, prefix) {
		return session.Claims{}, errors.New("boardapi: missing bearer token")
	}
	claims, err := h.sessions.Verify(strings.TrimPrefix(authHeader, prefix))
	if err != nil {
		return session.Claims{}, err
	}
	if h.isOwner != nil && !h.isOwner(claims.FileID) {
		return session.Claims{}, errWrongReplica
	}
	if h.observer != nil {
		h.observer.Observe(claims)
	}
	return claims, nil
}

// writeAuthError answers an authenticate failure: 421 for a
// wrong-replica rejection, which clients treat as transient and retry,
// and 401 for anything else.
func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, errWrongReplica) {
		http.Error(w, "wrong replica", http.StatusMisdirectedRequest)
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// writeWOPIError maps a GetFile failure to a response status.
func writeWOPIError(w http.ResponseWriter, err error) {
	if _, ok := errors.AsType[wopiclient.ErrTokenRejected](err); ok {
		http.Error(w, "the WOPI access token was rejected", http.StatusForbidden)
		return
	}
	if _, ok := errors.AsType[wopiclient.ErrNotFound](err); ok {
		http.Error(w, "board not found", http.StatusNotFound)
		return
	}
	slog.Error("boardapi: WOPI GetFile failed", "error", err)
	http.Error(w, "upstream WOPI host error", http.StatusBadGateway)
}

func writeJSON(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeJSONValue(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("boardapi: marshal JSON response", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, data)
}
