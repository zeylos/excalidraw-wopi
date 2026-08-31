package peers

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"time"
)

const dnsPollInterval = 5 * time.Second

// Resolver is the subset of *net.Resolver the poll loop needs. Tests
// inject a fake so they run without a real DNS lookup; internal/app also
// names this type for its own peerResolver test seam.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ticker is the subset of *time.Ticker the poll loop needs. Tests inject a
// fake so they control ticks by hand instead of waiting real time.
type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

func newRealTicker(d time.Duration) ticker {
	return realTicker{time.NewTicker(d)}
}

// Start begins the DNS poll loop for an enabled cluster: an immediate
// resolve, then one on every tick. Without the immediate resolve, a fresh
// replica would hold an empty snapshot for up to dnsPollInterval, fall
// back to Self, and wrongly claim every room as its own instead of
// proxying it away. Start is a no-op for a disabled cluster. Start and
// Close are safe to call from different goroutines, in either order, any
// number of times.
func (c *Cluster) Start() {
	if !c.enabled {
		return
	}
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return
	}
	c.started = true
	ctx, cancel := context.WithCancel(context.Background())
	c.pollCancel = cancel
	c.stopped = make(chan struct{})
	stopped := c.stopped
	c.mu.Unlock()

	go c.pollLoop(ctx, c.newTicker(dnsPollInterval), stopped)
}

// Close stops the DNS poll loop, if running, and waits for it to exit. It
// cancels an in-flight lookup rather than waiting out its own timeout. It
// is idempotent, safe to call on a disabled cluster, and safe to call
// before Start: a later Start then does nothing, so no loop is ever left
// running after Close.
func (c *Cluster) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	cancel := c.pollCancel
	stopped := c.stopped
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stopped != nil {
		<-stopped
	}
}

func (c *Cluster) pollLoop(ctx context.Context, t ticker, stopped chan struct{}) {
	defer close(stopped)
	defer t.Stop()
	c.resolveOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			c.resolveOnce(ctx)
		}
	}
}

// resolveOnce refreshes the peer snapshot from DNS. On a lookup error it
// keeps the previous snapshot and logs one warning, since a transient DNS
// hiccup must not make the replica stop routing to known peers. parent is
// the poll loop's own context, so Close cancels an in-flight lookup
// instead of this call blocking up to dnsPollInterval.
func (c *Cluster) resolveOnce(parent context.Context) {
	if c.afterResolve != nil {
		defer c.afterResolve()
	}

	ctx, cancel := context.WithTimeout(parent, dnsPollInterval)
	defer cancel()
	addrs, err := c.resolver.LookupIPAddr(ctx, c.dnsHost)
	if err != nil {
		slog.Warn("peers: dns lookup failed, keeping previous peer snapshot",
			"host", c.dnsHost, "error", err)
		return
	}

	seen := make(map[string]struct{}, len(addrs))
	peersList := make([]string, 0, len(addrs))
	for _, a := range addrs {
		// dns mode builds http:// peers by design; TLS between peers is
		// not supported in dns mode.
		u := "http://" + net.JoinHostPort(a.IP.String(), c.dnsPort)
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		peersList = append(peersList, u)
	}
	sort.Strings(peersList)
	c.snapshot.Store(&peersList)
}
