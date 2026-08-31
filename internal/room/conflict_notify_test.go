package room

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
)

// conflictNotifications records every OnConflictChange call, guarded by its
// own mutex since the Manager can call it from the background loop's
// goroutine while a test goroutine reads it.
type conflictNotifications struct {
	mu    sync.Mutex
	calls []conflictCall
}

type conflictCall struct {
	fileID      string
	inConflict  bool
	saveStalled bool
}

func (n *conflictNotifications) record(fileID string, inConflict, saveStalled bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, conflictCall{fileID: fileID, inConflict: inConflict, saveStalled: saveStalled})
}

func (n *conflictNotifications) snapshot() []conflictCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]conflictCall(nil), n.calls...)
}

// TestOnConflictChangeFiresOnEnterAndResolve covers the notifier wiring
// end to end: a foreign lock 409 fires the hook once with
// inConflict=true, and ResolveConflict fires it once more with
// inConflict=false, so a client-facing broadcast (internal/app's job)
// shows and then clears the conflict banner.
func TestOnConflictChangeFiresOnEnterAndResolve(t *testing.T) {
	f := newHostFixture(t, time.Hour)
	aliceToken := f.host.MintToken("alice", hostFileID)
	bobToken := f.host.MintToken("bob", hostFileID)
	clock := newFakeClock(time.Unix(0, 0))

	notifications := &conflictNotifications{}
	m := newTestManager(f.client, clock, WithOnConflictChange(notifications.record))

	const foreignLock = "some-other-editor-lock"
	if err := f.client.Lock(context.Background(), f.src, bobToken, foreignLock); err != nil {
		t.Fatalf("simulate a foreign lock: Lock() error = %v", err)
	}

	observe(m, f.src, hostFileID, "alice", aliceToken, true, clock.Now().Add(24*time.Hour))
	mustPutSceneAs(t, m, f.src, "scene-1", "alice")
	m.evaluateAll()

	if !m.Conflict(f.src) {
		t.Fatal("Conflict() = false, want true after a foreign lock 409")
	}

	if calls := notifications.snapshot(); len(calls) != 1 || calls[0] != (conflictCall{fileID: hostFileID, inConflict: true}) {
		t.Fatalf("notifications after entering conflict = %+v, want exactly one {fileID: %q, inConflict: true}", calls, hostFileID)
	}

	// A second background pass, still under the same foreign lock, must not
	// fire the hook again: the client already knows.
	m.evaluateAll()
	if calls := notifications.snapshot(); len(calls) != 1 {
		t.Fatalf("notifications after a repeat pass = %+v, want still exactly one (no duplicate fire)", calls)
	}

	if err := m.ResolveConflict(f.src, false); err != nil {
		t.Fatalf("ResolveConflict(reload) error = %v", err)
	}

	calls := notifications.snapshot()
	if len(calls) != 2 || calls[1] != (conflictCall{fileID: hostFileID, inConflict: false}) {
		t.Fatalf("notifications after ResolveConflict = %+v, want a second {fileID: %q, inConflict: false}", calls, hostFileID)
	}
}

// TestOnConflictChangeResolveOnNonConflictedRoomIsANoOp checks that
// ResolveConflict's existing no-op path (no tracked conflict) does not
// spuriously fire the hook.
func TestOnConflictChangeResolveOnNonConflictedRoomIsANoOp(t *testing.T) {
	client := newFakeClient()
	clock := newFakeClock(time.Unix(0, 0))
	notifications := &conflictNotifications{}
	m := newTestManager(client, clock, WithOnConflictChange(notifications.record))

	observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))

	if err := m.ResolveConflict(testWopiSrc, true); err != nil {
		t.Fatalf("ResolveConflict() on a non-conflicted room error = %v", err)
	}
	if calls := notifications.snapshot(); len(calls) != 0 {
		t.Fatalf("notifications = %+v, want none: the room was never in conflict", calls)
	}
}

// TestOnConflictChangeVersionDriftFiresOnce covers the checkVersion entry
// path, the fourth of the four conflict-entry sites.
func TestOnConflictChangeVersionDriftFiresOnce(t *testing.T) {
	f := newHostFixture(t, time.Hour)
	token := f.host.MintToken("alice", hostFileID)
	clock := newFakeClock(time.Unix(0, 0))
	notifications := &conflictNotifications{}
	m := newTestManager(f.client, clock, WithOnConflictChange(notifications.record))

	observe(m, f.src, hostFileID, "alice", token, true, clock.Now().Add(24*time.Hour))
	mustPutSceneAs(t, m, f.src, "scene-1", "alice")
	m.evaluateAll()
	if calls := notifications.snapshot(); len(calls) != 0 {
		t.Fatalf("notifications after a clean first save = %+v, want none", calls)
	}

	if _, err := f.client.PutFile(context.Background(), f.src, token, lockValueFor(hostFileID), []byte("scene-from-elsewhere")); err != nil {
		t.Fatalf("simulate an external PutFile: error = %v", err)
	}

	clock.Advance(hostadapter.LockRefreshInterval)
	m.evaluateAll()

	if calls := notifications.snapshot(); len(calls) != 1 || calls[0] != (conflictCall{fileID: hostFileID, inConflict: true}) {
		t.Fatalf("notifications after version drift = %+v, want exactly one {fileID: %q, inConflict: true}", calls, hostFileID)
	}
}

// TestOnReloadRequiredFiresOnlyOnReload covers WithOnReloadRequired:
// ResolveConflict(overwrite=false) fires it exactly once, with the
// room's fileID; overwrite=true never fires it, since every client
// already keeps its own scene in that case.
func TestOnReloadRequiredFiresOnlyOnReload(t *testing.T) {
	t.Run("reload fires once with the room's fileID", func(t *testing.T) {
		f := newHostFixture(t, time.Hour)
		aliceToken := f.host.MintToken("alice", hostFileID)
		bobToken := f.host.MintToken("bob", hostFileID)
		clock := newFakeClock(time.Unix(0, 0))

		var mu sync.Mutex
		var fired []string
		m := newTestManager(f.client, clock, WithOnReloadRequired(func(fileID string) {
			mu.Lock()
			fired = append(fired, fileID)
			mu.Unlock()
		}))

		const foreignLock = "some-other-editor-lock"
		if err := f.client.Lock(context.Background(), f.src, bobToken, foreignLock); err != nil {
			t.Fatalf("simulate a foreign lock: Lock() error = %v", err)
		}

		observe(m, f.src, hostFileID, "alice", aliceToken, true, clock.Now().Add(24*time.Hour))
		mustPutSceneAs(t, m, f.src, "scene-1", "alice")
		m.evaluateAll()
		if !m.Conflict(f.src) {
			t.Fatal("Conflict() = false, want true after a foreign lock 409")
		}

		if err := m.ResolveConflict(f.src, false); err != nil {
			t.Fatalf("ResolveConflict(reload) error = %v", err)
		}

		mu.Lock()
		got := append([]string(nil), fired...)
		mu.Unlock()
		if len(got) != 1 || got[0] != hostFileID {
			t.Fatalf("WithOnReloadRequired calls = %v, want exactly one call with fileID %q", got, hostFileID)
		}
	})

	t.Run("overwrite never fires it", func(t *testing.T) {
		f := newHostFixture(t, time.Hour)
		aliceToken := f.host.MintToken("alice", hostFileID)
		bobToken := f.host.MintToken("bob", hostFileID)
		clock := newFakeClock(time.Unix(0, 0))

		var mu sync.Mutex
		var fired []string
		m := newTestManager(f.client, clock, WithOnReloadRequired(func(fileID string) {
			mu.Lock()
			fired = append(fired, fileID)
			mu.Unlock()
		}))

		const foreignLock = "some-other-editor-lock"
		if err := f.client.Lock(context.Background(), f.src, bobToken, foreignLock); err != nil {
			t.Fatalf("simulate a foreign lock: Lock() error = %v", err)
		}

		observe(m, f.src, hostFileID, "alice", aliceToken, true, clock.Now().Add(24*time.Hour))
		mustPutSceneAs(t, m, f.src, "scene-1", "alice")
		m.evaluateAll()
		if !m.Conflict(f.src) {
			t.Fatal("Conflict() = false, want true after a foreign lock 409")
		}

		if err := f.client.Unlock(context.Background(), f.src, bobToken, foreignLock); err != nil {
			t.Fatalf("release the simulated foreign lock: Unlock() error = %v", err)
		}
		if err := m.ResolveConflict(f.src, true); err != nil {
			t.Fatalf("ResolveConflict(overwrite) error = %v", err)
		}

		mu.Lock()
		got := append([]string(nil), fired...)
		mu.Unlock()
		if len(got) != 0 {
			t.Fatalf("WithOnReloadRequired calls = %v, want none: overwrite must never trigger a reload", got)
		}
	})
}

// TestOnConflictChangeSaveStalledFiresOnceThenClears covers the
// saveStalled half of the notifier: it fires once a dirty room's
// failing streak reaches saveStalledWindow, stays silent on a repeat
// pass with no change, and clears once a save finally succeeds.
func TestOnConflictChangeSaveStalledFiresOnceThenClears(t *testing.T) {
	const token = "tok-a"
	client := newFakeClient()
	client.reject(token)
	clock := newFakeClock(time.Unix(0, 0))
	notifications := &conflictNotifications{}
	m := newTestManager(client, clock, WithOnConflictChange(notifications.record))

	observe(m, testWopiSrc, "f-1", "alice", token, true, clock.Now().Add(time.Hour))
	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")

	// The first failed attempt starts the streak; the stall window has not
	// elapsed yet, so no event fires.
	m.evaluateAll()
	if calls := notifications.snapshot(); len(calls) != 0 {
		t.Fatalf("notifications right after the first failure = %+v, want none", calls)
	}

	clock.Advance(saveStalledWindow)
	m.evaluateAll()

	calls := notifications.snapshot()
	if len(calls) != 1 || calls[0] != (conflictCall{fileID: "f-1", saveStalled: true}) {
		t.Fatalf("notifications once the stall window elapsed = %+v, want exactly one {saveStalled: true}", calls)
	}

	// A repeat pass, still failing, must not fire a duplicate event.
	m.evaluateAll()
	if calls := notifications.snapshot(); len(calls) != 1 {
		t.Fatalf("notifications after a repeat failing pass = %+v, want still exactly one (no duplicate fire)", calls)
	}

	client.unreject(token)
	clock.Advance(maxBackoff)
	m.evaluateAll()

	calls = notifications.snapshot()
	if len(calls) != 2 || calls[1] != (conflictCall{fileID: "f-1", saveStalled: false}) {
		t.Fatalf("notifications once the save succeeded = %+v, want a second {saveStalled: false}", calls)
	}
}
