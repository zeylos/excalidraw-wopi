// Package launch implements POST /launch, the action URL this service
// advertises in discovery. The WOPI host auto-submits a form to it to
// start an editor session.
package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/session"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

const (
	apiBase    = "/api"
	socketPath = "/socket.io"

	configScriptOpen  = `<script type="application/json" id="ew-config">`
	configScriptClose = `</script>`
	configPlaceholder = configScriptOpen + "{}" + configScriptClose

	// maxSessionTTL caps how far in the future a minted session's exp
	// claim can sit, whatever access_token_ttl the host sent.
	maxSessionTTL = 10 * time.Hour
)

// fileIDPattern mirrors internal/relay's roomIDPattern: the relay's
// join-room rejects any room id outside this charset, so a launch that
// derived a fileID outside it would succeed here and then never be able
// to join its own room. Kept as a local copy rather than an import, to
// keep this package independent of internal/relay.
var fileIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// CheckFileInfoer is the subset of wopiclient.Client the Handler calls to
// validate the WOPI access token live. wopiclient.Client satisfies it.
type CheckFileInfoer interface {
	CheckFileInfo(ctx context.Context, src, token string) (wopiclient.FileInfo, error)
}

// appConfig mirrors web/src/config.ts's AppConfig, the JSON blob the
// frontend reads out of the #ew-config script tag.
type appConfig struct {
	FileID       string `json:"fileId"`
	FileName     string `json:"fileName"`
	UserID       string `json:"userId"`
	UserName     string `json:"userName"`
	CanWrite     bool   `json:"canWrite"`
	SessionToken string `json:"sessionToken"`
	APIBase      string `json:"apiBase"`
	SocketPath   string `json:"socketPath"`
	// MaxImageBytes mirrors config.Config.MaxImageBytes: the one
	// env-configurable image size limit, enforced client-side by
	// web/src/utils/imageSizeLimit.ts on both the sending and receiving
	// paths, not just hardcoded to match the Go side's default.
	MaxImageBytes int64 `json:"maxImageBytes"`
	// TestHooks, omitted unless true, enables window.__excaTest client-side
	// (web/src/stores/useExcalidrawStore.ts). Only a Handler built with
	// WithTestHooks sets it; the normal launch path emits no such field.
	TestHooks bool `json:"testHooks,omitempty"`
}

// Handler serves POST /launch. It validates the WOPI access token with a
// live, signed CheckFileInfo call, mints a session JWT that seals the
// token, and serves the SPA's index.html with the launch config injected.
type Handler struct {
	client         CheckFileInfoer
	sessions       *session.Manager
	staticFS       fs.FS
	allowedOrigins map[string]struct{}
	cspFrameValue  string
	testHooks      bool
	maxImageBytes  int64
}

// Option configures optional Handler behavior.
type Option func(*Handler)

// WithTestHooks makes every launch through the Handler set testHooks:
// true in the injected config blob (web/src/config.ts's AppConfig),
// enabling window.__excaTest client-side. internal/app passes this for
// --fake-host dev mode and for EXCALIDRAW_WOPI_TEST_HOOKS=1 (the
// dockerized-Drive smoke suite); the normal launch path emits no
// testHooks field at all.
func WithTestHooks() Option {
	return func(h *Handler) { h.testHooks = true }
}

// New builds a launch Handler. client performs the signed CheckFileInfo
// call; sessions mints the session JWT; staticFS is the embedded web
// bundle that holds index.html. allowedOrigins lists the WOPI host
// origins (scheme://host[:port]) a WOPISrc must match; an empty or nil
// list rejects every request. maxImageBytes is
// config.Config.MaxImageBytes; every launch injects it into the frontend's
// config blob.
func New(client CheckFileInfoer, sessions *session.Manager, staticFS fs.FS, allowedOrigins []string, maxImageBytes int64, opts ...Option) *Handler {
	set := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if normalized, err := normalizeOrigin(o); err == nil {
			set[normalized] = struct{}{}
		}
	}
	h := &Handler{
		client:         client,
		sessions:       sessions,
		staticFS:       staticFS,
		allowedOrigins: set,
		cspFrameValue:  cspFrameAncestors(allowedOrigins),
		maxImageBytes:  maxImageBytes,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wopiSrc, err := parseWOPISrc(r.URL.Query().Get("WOPISrc"))
	if err != nil {
		http.Error(w, "invalid or missing WOPISrc: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !h.originAllowed(wopiSrc) {
		slog.Warn("launch: WOPISrc origin is not in the allowlist", "wopiSrc", wopiSrc)
		http.Error(w, "WOPISrc origin is not allowed", http.StatusForbidden)
		return
	}
	fileID, err := fileIDFromWOPISrc(wopiSrc)
	if err != nil {
		http.Error(w, "invalid WOPISrc: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid launch form body", http.StatusBadRequest)
		return
	}
	accessToken := r.PostForm.Get("access_token")
	expiresAt, err := parseAccessTokenTTL(r.PostForm.Get("access_token_ttl"))
	if accessToken == "" || err != nil {
		http.Error(w, "missing or invalid access_token or access_token_ttl", http.StatusBadRequest)
		return
	}
	if maxExpiry := time.Now().Add(maxSessionTTL); expiresAt.After(maxExpiry) {
		expiresAt = maxExpiry
	}

	info, err := h.client.CheckFileInfo(r.Context(), wopiSrc, accessToken)
	if err != nil {
		writeCheckFileInfoError(w, err)
		return
	}

	sessionToken, err := h.sessions.Mint(session.MintParams{
		FileID:      fileID,
		WOPISrc:     wopiSrc,
		UserID:      info.UserID,
		UserName:    info.UserFriendlyName,
		CanWrite:    info.UserCanWrite,
		AccessToken: accessToken,
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		slog.Error("launch: mint session token", "error", err)
		http.Error(w, "internal error", http.StatusBadGateway)
		return
	}

	page, err := h.renderPage(appConfig{
		FileID:        fileID,
		FileName:      info.BaseFileName,
		UserID:        info.UserID,
		UserName:      info.UserFriendlyName,
		CanWrite:      info.UserCanWrite,
		SessionToken:  sessionToken,
		APIBase:       apiBase,
		SocketPath:    socketPath,
		TestHooks:     h.testHooks,
		MaxImageBytes: h.maxImageBytes,
	})
	if err != nil {
		slog.Error("launch: render page", "error", err)
		http.Error(w, "internal error", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// A WOPI host frames the editor from its own page. Drive serves that
	// page from the same origin as its WOPI API, so the WOPI allowlist is
	// the correct frame-ancestors default. A deployment that needs a
	// separate value gets its own config var.
	w.Header().Set("Content-Security-Policy", "frame-ancestors "+h.cspFrameValue)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

// writeCheckFileInfoError maps a CheckFileInfo failure to a launch
// response: a rejected token is a client-fixable 403, a proof failure
// and every other error are a 502, since both mean the service could
// not complete the launch against the WOPI host.
func writeCheckFileInfoError(w http.ResponseWriter, err error) {
	if _, ok := errors.AsType[wopiclient.ErrTokenRejected](err); ok {
		http.Error(w, "the WOPI access token was rejected", http.StatusForbidden)
		return
	}

	if _, ok := errors.AsType[wopiclient.ErrProofRejected](err); ok {
		slog.Error("launch: CheckFileInfo failed proof verification; the proof key likely mismatches the one the host's discovery config holds",
			"error", err)
		http.Error(w, "upstream WOPI host rejected the request", http.StatusBadGateway)
		return
	}

	slog.Error("launch: CheckFileInfo failed", "error", err)
	http.Error(w, "upstream WOPI host error", http.StatusBadGateway)
}

// renderPage injects cfg into a copy of index.html's #ew-config script tag.
func (h *Handler) renderPage(cfg appConfig) ([]byte, error) {
	page, err := fs.ReadFile(h.staticFS, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}

	// json.Marshal escapes '<', '>', and '&' to \uXXXX by default, so a
	// host-supplied fileName can never inject a literal </script>.
	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal launch config: %w", err)
	}

	if !bytes.Contains(page, []byte(configPlaceholder)) {
		return nil, errors.New("index.html is missing the ew-config placeholder")
	}
	replacement := append(append([]byte(configScriptOpen), payload...), configScriptClose...)
	return bytes.Replace(page, []byte(configPlaceholder), replacement, 1), nil
}

func parseWOPISrc(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("WOPISrc is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse WOPISrc: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("WOPISrc must be an absolute URL")
	}
	return raw, nil
}

// fileIDFromWOPISrc reads the WOPI file id off the end of the WOPISrc
// path, e.g. .../api/v1.0/wopi/files/{id} -> {id}, and rejects an id
// outside fileIDPattern's charset: the relay's join-room would refuse it
// later, after the session is already minted.
func fileIDFromWOPISrc(wopiSrc string) (string, error) {
	u, err := url.Parse(wopiSrc)
	if err != nil {
		return "", fmt.Errorf("parse WOPISrc: %w", err)
	}
	id := path.Base(u.Path)
	if id == "" || id == "." || id == "/" {
		return "", errors.New("WOPISrc carries no file id")
	}
	if !fileIDPattern.MatchString(id) {
		return "", fmt.Errorf("WOPISrc file id %q has an unsupported character set", id)
	}
	return id, nil
}

// parseAccessTokenTTL reads access_token_ttl, an absolute Unix epoch
// timestamp in milliseconds, not a duration. A non-future value is
// rejected: it cannot seed a usable session, and
// letting it through would mint a session that starts already expired.
func parseAccessTokenTTL(raw string) (time.Time, error) {
	millis, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse access_token_ttl: %w", err)
	}
	ttl := time.UnixMilli(millis)
	if !ttl.After(time.Now()) {
		return time.Time{}, errors.New("access_token_ttl must be in the future")
	}
	return ttl, nil
}

// originAllowed reports whether wopiSrc's origin is in the configured
// allowlist.
func (h *Handler) originAllowed(wopiSrc string) bool {
	origin, err := normalizeOrigin(wopiSrc)
	if err != nil {
		return false
	}
	_, ok := h.allowedOrigins[origin]
	return ok
}

// normalizeOrigin extracts scheme://host[:port] from raw (a bare origin
// or a full URL; any path or query is ignored) and lowercases it, so an
// origin comparison does not depend on how the operator or the host
// happened to case it.
func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse origin: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("origin must include a scheme and a host")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

// cspFrameAncestors builds the frame-ancestors CSP directive value from
// the configured origins, in the order given. An empty list denies
// framing altogether, consistent with an empty allowlist rejecting every
// launch.
func cspFrameAncestors(origins []string) string {
	if len(origins) == 0 {
		return "'none'"
	}
	return strings.Join(origins, " ")
}
