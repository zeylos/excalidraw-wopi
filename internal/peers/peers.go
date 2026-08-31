// Package peers implements multi-replica awareness for excalidraw-wopi.
//
// Each file's collaboration room lives on exactly one replica, the owner,
// picked as a pure function of (fileID, peer set) via rendezvous hashing.
// A replica that accepts a request for a room it does not own reverse-proxies
// it to the owner. Peer membership needs no consensus: a divergent view
// between replicas is tolerated by design (a transient dual owner is safe,
// since the WOPI lock value is deterministic and clients re-sync), so
// discovery here is best-effort, not strongly consistent.
package peers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures a Cluster.
type Config struct {
	// DNS is the raw EXCALIDRAW_WOPI_DNS_PEERS value, "<host>:<port>".
	// Empty disables the cluster.
	DNS string
	// Self is this replica's own advertised URL, e.g. "http://10.0.3.7:8080".
	Self string
}

// Cluster routes a request for a file to the replica that owns it.
type Cluster struct {
	enabled bool
	self    string

	dnsHost string
	dnsPort string

	resolver  Resolver
	newTicker func(time.Duration) ticker

	// mu guards started, closed, pollCancel, and stopped. Start and Close
	// can run from different goroutines in either order; mu makes each
	// pair member idempotent and race-free regardless of call order.
	mu         sync.Mutex
	started    bool
	closed     bool
	pollCancel context.CancelFunc
	stopped    chan struct{}

	snapshot atomic.Pointer[[]string]

	transport http.RoundTripper

	// proxyOnce builds proxy on first use: one *httputil.ReverseProxy per
	// Cluster, reused across every proxied request instead of allocated
	// per request. See middleware.go.
	proxyOnce sync.Once
	proxy     *httputil.ReverseProxy

	// afterResolve, when set, runs at the end of every resolveOnce call.
	// It exists only so tests can wait for a poll to finish instead of
	// sleeping; production code leaves it nil.
	afterResolve func()
}

// Option configures optional Cluster behavior.
type Option func(*Cluster)

// WithTransport overrides the RoundTripper the reverse-proxy middleware
// uses. Test seam: lets a test dispatch a proxied request without a real
// TCP dial. Defaults to a cloned http.DefaultTransport.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Cluster) { c.transport = rt }
}

// WithResolver overrides the DNS resolver the poll loop uses. Test seam:
// lets a test inject a fake resolver instead of a real DNS lookup.
// Defaults to net.DefaultResolver.
func WithResolver(r Resolver) Option {
	return func(c *Cluster) { c.resolver = r }
}

// New parses cfg and returns a Cluster. It returns an error only for an
// unparsable DNS spec or self URL. The cluster does not resolve anything
// yet at this point, so a transient DNS outage at boot is never fatal
// here; Start begins the actual polling.
func New(cfg Config, opts ...Option) (*Cluster, error) {
	var c *Cluster

	if cfg.DNS == "" {
		c = &Cluster{enabled: false}
	} else {
		host, port, err := parseDNSSpec(cfg.DNS)
		if err != nil {
			return nil, err
		}
		self, err := normalizePeerURL(cfg.Self)
		if err != nil {
			return nil, fmt.Errorf("peers: invalid self URL %q: %w", cfg.Self, err)
		}
		// Self does not have to appear in the resolved set. A Kubernetes
		// readiness gate keeps a starting pod out of the headless Service's
		// DNS answer, so a fresh replica can legitimately be missing from
		// every snapshot: it then never wins rendezvous, owns nothing, and
		// proxies every request elsewhere, until it becomes ready.
		c = &Cluster{
			enabled:   true,
			self:      self,
			dnsHost:   host,
			dnsPort:   port,
			resolver:  net.DefaultResolver,
			newTicker: newRealTicker,
			transport: newTransport(),
		}
		empty := []string{}
		c.snapshot.Store(&empty)
	}

	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Enabled reports whether multi-replica routing is on.
func (c *Cluster) Enabled() bool {
	return c.enabled
}

// Owner returns the URL of the replica that owns fileID: "" when the
// cluster is disabled, Self when the peer snapshot is empty (serve
// locally rather than fail), otherwise the rendezvous-hash winner.
func (c *Cluster) Owner(fileID string) string {
	if !c.Enabled() {
		return ""
	}
	peersList := c.currentSnapshot()
	if len(peersList) == 0 {
		return c.self
	}
	return rendezvousOwner(fileID, peersList)
}

// IsSelf reports whether this replica owns fileID. It is always true when
// the cluster is disabled.
func (c *Cluster) IsSelf(fileID string) bool {
	if !c.Enabled() {
		return true
	}
	return c.Owner(fileID) == c.self
}

func (c *Cluster) currentSnapshot() []string {
	p := c.snapshot.Load()
	if p == nil {
		return nil
	}
	return *p
}

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// An ALPN-negotiated HTTP/2 connection rejects "Connection: Upgrade", so
	// HTTP/2 to a peer would break the reverse proxy's websocket upgrade.
	// Peer proxy traffic is HTTP/1.1 by design.
	t.ForceAttemptHTTP2 = false
	return t
}
