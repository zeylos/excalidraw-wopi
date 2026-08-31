package peers

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// hopHeader marks a request this cluster already forwarded once. A second
// hop that still does not land on the owner means the replicas disagree on
// membership; the request must fail loudly instead of bouncing forever.
const hopHeader = "X-Excalidraw-Wopi-Hop"

// proxyTargetKey is the request context key Middleware uses to pass this
// request's owner URL to the cluster's one shared reverse proxy (see
// reverseProxy and rewrite below).
type proxyTargetKey struct{}

// Middleware reverse-proxies a request whose "room" query parameter names
// a file this replica does not own to the replica that does. It passes
// the request to next unchanged when the cluster is disabled, when no
// room hint is present, or when this replica already owns the room; token-
// level enforcement of the room ownership is a caller concern, not this
// middleware's.
func (c *Cluster) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.Enabled() {
			next.ServeHTTP(w, r)
			return
		}

		// The hop header is client-settable. A stray value must not force
		// a 421, so only the exact value that rewriteProxyRequest sets
		// counts as the loop marker. The middleware deletes the incoming
		// header, and rewriteProxyRequest sets a fresh one on the
		// outbound request.
		hopped := r.Header.Get(hopHeader) == "1"
		r.Header.Del(hopHeader)

		room := r.URL.Query().Get("room")
		if room == "" {
			next.ServeHTTP(w, r)
			return
		}
		if c.IsSelf(room) {
			next.ServeHTTP(w, r)
			return
		}

		target := c.Owner(room)
		if hopped {
			slog.Error("peers: misdirected request, replicas disagree on room ownership",
				"self", c.self, "target", target, "room", room)
			http.Error(w, "misdirected request: peer ownership disagreement", http.StatusMisdirectedRequest)
			return
		}

		targetURL, err := url.Parse(target)
		if err != nil {
			slog.Error("peers: unparsable owner URL", "target", target, "error", err)
			http.Error(w, "internal routing error", http.StatusBadGateway)
			return
		}

		ctx := context.WithValue(r.Context(), proxyTargetKey{}, targetURL)
		c.reverseProxy().ServeHTTP(w, r.WithContext(ctx))
	})
}

// reverseProxy returns the cluster's one reusable proxy, building it on
// first use so every proxied request shares it instead of allocating a
// fresh *httputil.ReverseProxy per request. The per-request target
// travels through the request context (proxyTargetKey,
// set above) since rewrite has no other way to learn it.
func (c *Cluster) reverseProxy() *httputil.ReverseProxy {
	c.proxyOnce.Do(func() {
		c.proxy = &httputil.ReverseProxy{
			Transport:     c.transport,
			FlushInterval: -1, // keeps ReverseProxy's native websocket upgrade support working.
			Rewrite:       c.rewriteProxyRequest,
			ErrorHandler:  c.proxyErrorHandler,
		}
	})
	return c.proxy
}

// rewriteProxyRequest routes the outbound request to this request's owner
// target (set in the context by Middleware), preserves the inbound Host
// header the way httputil.NewSingleHostReverseProxy did, and tags the
// request with hopHeader so a routing loop is detected, not bounced
// forever.
func (c *Cluster) rewriteProxyRequest(pr *httputil.ProxyRequest) {
	target, ok := pr.In.Context().Value(proxyTargetKey{}).(*url.URL)
	if !ok {
		// Middleware always sets proxyTargetKey before dispatch; reaching
		// this means a caller invoked the proxy directly, skipping it.
		slog.Error("peers: proxy request carries no target; Middleware was bypassed")
		return
	}
	pr.SetURL(target)
	pr.Out.Host = pr.In.Host
	pr.Out.Header.Set(hopHeader, "1")
}

// proxyErrorHandler logs a failed proxy attempt and answers the client
// with a 502, the same behavior the former per-request ErrorHandler gave.
func (c *Cluster) proxyErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	target, _ := r.Context().Value(proxyTargetKey{}).(*url.URL)
	slog.Error("peers: proxy request failed", "target", target, "error", err)
	http.Error(w, "peer proxy request failed", http.StatusBadGateway)
}
