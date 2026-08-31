package peers

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeTicker struct {
	c       chan time.Time
	stopped bool
}

func newFakeTicker() *fakeTicker { return &fakeTicker{c: make(chan time.Time)} }

func (f *fakeTicker) C() <-chan time.Time { return f.c }
func (f *fakeTicker) Stop()               { f.stopped = true }

type fakeResolver struct {
	mu    sync.Mutex
	addrs []net.IPAddr
	err   error
}

func (f *fakeResolver) setAddrs(addrs []net.IPAddr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrs = addrs
	f.err = nil
}

func (f *fakeResolver) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]net.IPAddr, len(f.addrs))
	copy(out, f.addrs)
	return out, nil
}

func newTestDNSCluster(resolver Resolver, ft *fakeTicker) *Cluster {
	c := &Cluster{
		enabled:   true,
		self:      "http://self:8080",
		dnsHost:   "svc.local",
		dnsPort:   "8080",
		resolver:  resolver,
		newTicker: func(time.Duration) ticker { return ft },
		transport: newTransport(),
	}
	empty := []string{}
	c.snapshot.Store(&empty)
	return c
}

// TestStartResolvesImmediatelyWithoutATick pins that Start populates the
// snapshot right away: without this, a fresh replica would fall back to
// Self and wrongly claim every room until the first tick fired.
func TestStartResolvesImmediatelyWithoutATick(t *testing.T) {
	resolver := &fakeResolver{}
	resolver.setAddrs([]net.IPAddr{{IP: net.ParseIP("10.0.0.1")}})
	ft := newFakeTicker()
	c := newTestDNSCluster(resolver, ft)

	resolved := make(chan struct{}, 1)
	c.afterResolve = func() { resolved <- struct{}{} }

	c.Start()
	defer c.Close()

	<-resolved // Start's own immediate resolve; no tick sent.

	want := []string{"http://10.0.0.1:8080"}
	if got := c.currentSnapshot(); !equalStrings(got, want) {
		t.Fatalf("snapshot right after Start() = %v, want %v", got, want)
	}
}

func TestPollLoopUpdatesSnapshotOnChange(t *testing.T) {
	resolver := &fakeResolver{}
	resolver.setAddrs([]net.IPAddr{{IP: net.ParseIP("10.0.0.1")}})
	ft := newFakeTicker()
	c := newTestDNSCluster(resolver, ft)

	resolved := make(chan struct{}, 1)
	c.afterResolve = func() { resolved <- struct{}{} }

	c.Start()
	defer c.Close()
	<-resolved // drain Start's immediate resolve before driving ticks by hand.

	resolver.setAddrs([]net.IPAddr{
		{IP: net.ParseIP("10.0.0.1")},
		{IP: net.ParseIP("10.0.0.2")},
	})
	ft.c <- time.Now()
	<-resolved

	want := []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080"}
	if got := c.currentSnapshot(); !equalStrings(got, want) {
		t.Fatalf("snapshot after change = %v, want %v", got, want)
	}
}

func TestPollLoopKeepsSnapshotOnResolutionError(t *testing.T) {
	resolver := &fakeResolver{}
	resolver.setAddrs([]net.IPAddr{{IP: net.ParseIP("10.0.0.1")}})
	ft := newFakeTicker()
	c := newTestDNSCluster(resolver, ft)

	resolved := make(chan struct{}, 1)
	c.afterResolve = func() { resolved <- struct{}{} }

	c.Start()
	defer c.Close()
	<-resolved // drain Start's immediate resolve.
	want := []string{"http://10.0.0.1:8080"}
	if got := c.currentSnapshot(); !equalStrings(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}

	resolver.setErr(errors.New("lookup failed"))
	ft.c <- time.Now()
	<-resolved

	if got := c.currentSnapshot(); !equalStrings(got, want) {
		t.Fatalf("snapshot after a resolution error = %v, want unchanged %v", got, want)
	}
}

func TestCloseStopsPollLoop(t *testing.T) {
	resolver := &fakeResolver{}
	resolver.setAddrs([]net.IPAddr{{IP: net.ParseIP("10.0.0.1")}})
	ft := newFakeTicker()
	c := newTestDNSCluster(resolver, ft)

	c.Start()
	c.Close()

	if !ft.stopped {
		t.Error("Close() must stop the injected ticker")
	}

	c.Close() // must not block or panic a second time.
}

// TestCloseBeforeStartPreventsALaterStart pins that the pair is safe in
// either order: a Close that races ahead of Start must not leave Start
// free to launch a loop nothing will ever stop again.
func TestCloseBeforeStartPreventsALaterStart(t *testing.T) {
	resolver := &fakeResolver{}
	resolver.setAddrs([]net.IPAddr{{IP: net.ParseIP("10.0.0.1")}})
	ft := newFakeTicker()
	c := newTestDNSCluster(resolver, ft)

	c.Close()
	c.Start() // must be a no-op: Close already ran.

	if got := c.currentSnapshot(); len(got) != 0 {
		t.Errorf("snapshot after Close then Start() = %v, want empty (Start must not run)", got)
	}
	if ft.stopped {
		t.Error("ticker.Stop() was called, but Start() should never have created a loop to stop")
	}
	c.Close() // still idempotent.
}

func TestStartAndCloseAreNoOpsForDisabledCluster(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New() returned an error: %v", err)
	}
	c.Start() // must not panic or start a goroutine.
	c.Close() // must not block.
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
