package room

import (
	"testing"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
)

// TestSaveStalledImmediateOnWriteLoss covers the durable-write-loss case:
// a save pass where every tried token gets
// wopiclient.ErrNoWriteAccess must report saveStalled at once, not after
// saveStalledWindow (5 minutes). It then checks that a later pass with an
// empty ladder (errNoUsableToken, since every token is now marked
// canWrite=false) keeps saveStalled=true, and that a fresh write token
// joining and saving successfully clears it again.
func TestSaveStalledImmediateOnWriteLoss(t *testing.T) {
	const tokA, tokB = "tok-a", "tok-b"
	client := newFakeClient()
	client.noWriteTokens[tokA] = true
	client.noWriteTokens[tokB] = true
	clock := newFakeClock(time.Unix(0, 0))
	notifications := &conflictNotifications{}
	m := newTestManager(client, clock, WithOnConflictChange(notifications.record))

	observe(m, testWopiSrc, "f-1", "alice", tokA, true, clock.Now().Add(time.Hour))
	observe(m, testWopiSrc, "f-1", "bob", tokB, true, clock.Now().Add(time.Hour))
	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")

	// Both tracked tokens 401 on this single pass, well before
	// saveStalledWindow has any chance to elapse.
	m.evaluateAll()

	calls := notifications.snapshot()
	if len(calls) != 1 || calls[0] != (conflictCall{fileID: "f-1", saveStalled: true}) {
		t.Fatalf("notifications after an all-401 pass = %+v, want exactly one {saveStalled: true}", calls)
	}
	if !m.SaveStalled(testWopiSrc) {
		t.Fatal("SaveStalled() = false, want true right after every tracked token lost write access")
	}

	// Every token is now canWrite=false, so the next pass finds an empty
	// ladder (errNoUsableToken). That must not clear the flag an earlier
	// pass already set.
	clock.Advance(initialBackoff)
	m.evaluateAll()

	if !m.SaveStalled(testWopiSrc) {
		t.Fatal("SaveStalled() = false after an empty-ladder pass, want still true")
	}
	if calls := notifications.snapshot(); len(calls) != 1 {
		t.Fatalf("notifications after the empty-ladder pass = %+v, want still exactly one (no duplicate fire)", calls)
	}

	// A fresh write-capable token joins and this time the save goes
	// through: writeLost, and with it saveStalled, must clear.
	observe(m, testWopiSrc, "f-1", "carol", "tok-c", true, clock.Now().Add(time.Hour))
	clock.Advance(maxBackoff)
	m.evaluateAll()

	if m.SaveStalled(testWopiSrc) {
		t.Fatal("SaveStalled() = true after a fresh token saved successfully, want false")
	}
	calls = notifications.snapshot()
	if len(calls) != 2 || calls[1] != (conflictCall{fileID: "f-1", saveStalled: false}) {
		t.Fatalf("notifications once the save recovered = %+v, want a second {saveStalled: false}", calls)
	}
}

// TestSaveStalledWriteLossNotSetOnMixedFailure covers the negative case:
// a pass where one token 401s but another fails a
// different way (here, a network-style fault on PutFile) must not set
// writeLost. The plain saveStalledWindow path still applies to that pass.
func TestSaveStalledWriteLossNotSetOnMixedFailure(t *testing.T) {
	const tokA, tokB = "tok-a", "tok-b"
	client := newFakeClient()
	client.noWriteTokens[tokA] = true
	client.genericFailTokens[tokB] = true
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(client, clock)

	observe(m, testWopiSrc, "f-1", "alice", tokA, true, clock.Now().Add(time.Hour))
	observe(m, testWopiSrc, "f-1", "bob", tokB, true, clock.Now().Add(time.Hour))
	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")

	m.evaluateAll()

	if m.SaveStalled(testWopiSrc) {
		t.Fatal("SaveStalled() = true right after a mixed 401/network-fault pass, want false: the window has not elapsed")
	}

	// The window path still applies: once saveStalledWindow elapses on
	// this same failing streak, saveStalled must still turn true.
	clock.Advance(saveStalledWindow)
	m.evaluateAll()
	if !m.SaveStalled(testWopiSrc) {
		t.Fatal("SaveStalled() = false once saveStalledWindow elapsed, want true (the window path, not writeLost)")
	}
}

// TestSaveStalledImmediateOnWriteLossViaPutFile401 covers performSave's
// other ErrNoWriteAccess branch: Lock succeeds, but PutFile itself 401s.
// The other tests in this file 401 on Lock instead, so this pins the
// PutFile branch's own writeLost tracking.
func TestSaveStalledImmediateOnWriteLossViaPutFile401(t *testing.T) {
	const tokA, tokB = "tok-a", "tok-b"
	client := newFakeClient()
	client.noWritePutTokens[tokA] = true
	client.noWritePutTokens[tokB] = true
	clock := newFakeClock(time.Unix(0, 0))
	notifications := &conflictNotifications{}
	m := newTestManager(client, clock, WithOnConflictChange(notifications.record))

	observe(m, testWopiSrc, "f-1", "alice", tokA, true, clock.Now().Add(time.Hour))
	observe(m, testWopiSrc, "f-1", "bob", tokB, true, clock.Now().Add(time.Hour))
	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")

	m.evaluateAll()

	if got := client.putCount(); got != 2 {
		t.Fatalf("putCount = %d, want 2: both candidates must reach PutFile after a successful Lock", got)
	}
	if !m.SaveStalled(testWopiSrc) {
		t.Fatal("SaveStalled() = false, want true: every candidate's PutFile call got ErrNoWriteAccess")
	}
	calls := notifications.snapshot()
	if len(calls) != 1 || calls[0] != (conflictCall{fileID: "f-1", saveStalled: true}) {
		t.Fatalf("notifications after an all-401-on-PutFile pass = %+v, want exactly one {saveStalled: true}", calls)
	}
}

// TestRefreshLockSetsWriteLostImmediately covers the refresh-loop gap: a
// clean, live room whose lock refresh 401s on every candidate must set
// writeLost right there, not only on the next performSave pass (an empty
// ladder by itself does not prove write access was lost). saveStalled
// still only shows once the room turns dirty, since saveStalledLocked
// requires rs.dirty; this checks that once it does, saveStalled reports
// at once, using the flag refreshLock already set.
func TestRefreshLockSetsWriteLostImmediately(t *testing.T) {
	const tokA, tokB = "tok-a", "tok-b"
	client := newFakeClient()
	clock := newFakeClock(time.Unix(0, 0))
	notifications := &conflictNotifications{}
	m := newTestManager(client, clock, WithOnConflictChange(notifications.record))

	observe(m, testWopiSrc, "f-1", "alice", tokA, true, clock.Now().Add(24*time.Hour))
	observe(m, testWopiSrc, "f-1", "bob", tokB, true, clock.Now().Add(24*time.Hour))

	// Put the room straight into "already holding the lock", so this
	// pass takes the refresh path directly, instead of spending a pass
	// on the initial lock acquisition first.
	m.mu.Lock()
	rs := m.rooms[testWopiSrc]
	rs.haveLock = true
	rs.lastLockAt = clock.Now()
	m.mu.Unlock()

	client.noWriteTokens[tokA] = true
	client.noWriteTokens[tokB] = true
	clock.Advance(hostadapter.LockRefreshInterval)
	m.evaluateAll()

	if got := len(client.refreshCalls); got != 2 {
		t.Fatalf("RefreshLock calls = %d, want 2 (both candidates tried)", got)
	}
	if m.SaveStalled(testWopiSrc) {
		t.Fatal("SaveStalled() = true on a clean room, want false: saveStalled requires the room to be dirty")
	}

	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")
	m.evaluateAll()

	if !m.SaveStalled(testWopiSrc) {
		t.Fatal("SaveStalled() = false right after the room turned dirty, want true: the refresh pass's writeLost must report at once")
	}
	calls := notifications.snapshot()
	if len(calls) != 1 || calls[0] != (conflictCall{fileID: "f-1", saveStalled: true}) {
		t.Fatalf("notifications once the room turned dirty = %+v, want exactly one {saveStalled: true}", calls)
	}
}
