package room

import (
	"testing"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

// TestFirstSaveFallsBackToUnlockedPutFile exercises performSave's
// unlocked-PutFile fallback: on a room's very first ever save attempt, a
// Lock failure that is neither a token rejection nor a lock conflict (a
// transport error, an unrecognized host fault) falls through to a plain,
// unlocked PutFile rather than giving up. A later save on the same room
// must not take this path (see the second sub-test): the fallback exists
// only for the empty-file edge case, where an unlocked PutFile can still
// succeed.
func TestFirstSaveFallsBackToUnlockedPutFile(t *testing.T) {
	client := newFakeClient()
	client.failLockGeneric = true
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(client, clock)
	observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))

	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")
	m.evaluateAll()

	if got := client.putCount(); got != 1 {
		t.Fatalf("putCount = %d, want 1 (the unlocked fallback should have saved)", got)
	}
	put := client.lastPut()
	if put.lock != "" {
		t.Fatalf("fallback PutFile lock = %q, want empty (unlocked)", put.lock)
	}
	if string(put.body) != "scene-1" {
		t.Fatalf("fallback PutFile body = %q, want scene-1", put.body)
	}

	t.Run("a later save does not take the unlocked fallback", func(t *testing.T) {
		clock.Advance(2 * time.Minute) // clear the ServerSaveInterval throttle
		mustPutSceneAs(t, m, testWopiSrc, "scene-2", "alice")
		m.evaluateAll()

		// Lock still fails generically every time; with no first-attempt
		// fallback available, the save has nothing that works and backs off.
		if got := client.putCount(); got != 1 {
			t.Fatalf("putCount = %d, want still 1: a later save must not use the unlocked fallback", got)
		}
	})
}

// TestForeignLockOnEmptyLockRetryEntersConflict covers ensureLocked's
// empty-lock retry edge: a first LOCK 409ing with an empty CurrentLock
// means the old lock had already expired, so ensureLocked retries once
// with a plain LOCK. If that retry itself 409s again, this time against a
// genuine foreign lock value, the room must enter conflict state exactly
// like the default branch does for a foreign value found on the first
// attempt: rs.conflict must not stay false, or the room would hot-retry
// LOCK+PUT every poll interval with no conflict-state event ever reaching
// clients.
func TestForeignLockOnEmptyLockRetryEntersConflict(t *testing.T) {
	const token = "tok-a"
	const foreignLock = "some-other-editor-lock"
	client := newFakeClient()
	client.queueLockConflicts(token,
		wopiclient.ErrLockConflict{CurrentLock: ""},
		wopiclient.ErrLockConflict{CurrentLock: foreignLock},
	)
	clock := newFakeClock(time.Unix(0, 0))
	notifications := &conflictNotifications{}
	m := newTestManager(client, clock, WithOnConflictChange(notifications.record))

	observe(m, testWopiSrc, "f-1", "alice", token, true, clock.Now().Add(time.Hour))
	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")
	m.evaluateAll()

	if !m.Conflict(testWopiSrc) {
		t.Fatal("Conflict() = false, want true: a foreign lock found on the empty-lock retry must enter conflict")
	}
	if got := client.lockCount(); got != 2 {
		t.Fatalf("lockCount = %d, want 2 (the initial LOCK plus the empty-lock retry)", got)
	}
	if got := client.putCount(); got != 0 {
		t.Fatalf("putCount = %d, want 0: a conflicted room must not save", got)
	}

	m.mu.Lock()
	gotForeignLock := m.rooms[testWopiSrc].conflictForeignLock
	m.mu.Unlock()
	if gotForeignLock != foreignLock {
		t.Fatalf("conflictForeignLock = %q, want %q", gotForeignLock, foreignLock)
	}

	calls := notifications.snapshot()
	if len(calls) != 1 || calls[0] != (conflictCall{fileID: "f-1", inConflict: true}) {
		t.Fatalf("notifications = %+v, want exactly one {inConflict: true}: the Manager must fire a conflict-state event, not retry silently", calls)
	}
}
