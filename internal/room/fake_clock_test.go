package room

import (
	"sync"
	"time"
)

// fakeClock is a Clock a test drives by hand: Now never moves on its own,
// only when a test calls Advance. Every save-throttle, lock-refresh,
// backoff, and close-grace test in this package drives the Manager's
// schedule through it, with no reliance on wall-clock sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
