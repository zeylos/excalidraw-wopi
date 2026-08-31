package room

import (
	"testing"
	"time"
)

// TestTokenExpiryWatchWarnsOncePerToken covers the TTL watch: it fires
// the hook once a tracked token drops under tokenExpiryWarnWindow of its
// own expiry, and it does not fire again on a later pass for the same
// token.
func TestTokenExpiryWatchWarnsOncePerToken(t *testing.T) {
	client := newFakeClient()
	clock := newFakeClock(time.Unix(0, 0))

	type warning struct{ fileID, userID string }
	var warnings []warning
	m := NewManager(client, Config{}, clock, WithOnTokenExpiring(func(fileID, userID string) {
		warnings = append(warnings, warning{fileID, userID})
	}))

	expiresAt := clock.Now().Add(tokenExpiryWarnWindow + time.Minute)
	observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, expiresAt)

	m.evaluateAll()
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none: still a minute outside the warn window", warnings)
	}

	clock.Advance(2 * time.Minute) // now 1 minute inside the 10 min warn window
	m.evaluateAll()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	if warnings[0] != (warning{fileID: "f-1", userID: "alice"}) {
		t.Fatalf("warning = %+v, want {f-1 alice}", warnings[0])
	}

	clock.Advance(time.Minute)
	m.evaluateAll()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want still exactly one: no repeat warning for the same token", warnings)
	}
}
