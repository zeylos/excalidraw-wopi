package room

import "time"

// Clock is the time source every time-based decision in this package
// reads. Production uses SystemClock; tests use a fake that advances on
// command, so the save throttle, the lock refresh cadence, and the
// backoff schedule are all deterministic to test.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock is the production Clock.
var SystemClock Clock = systemClock{}
