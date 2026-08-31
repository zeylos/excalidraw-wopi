package room

import (
	"context"
	"testing"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/boardapi"
	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/session"
)

const testWopiSrc = "https://drive.example/wopi/files/f-1"

func observe(m *Manager, wopiSrc, fileID, userID, token string, canWrite bool, expiresAt time.Time) {
	m.Observe(session.Claims{
		FileID:      fileID,
		WOPISrc:     wopiSrc,
		UserID:      userID,
		UserName:    userID,
		CanWrite:    canWrite,
		AccessToken: token,
		ExpiresAt:   expiresAt,
	})
}

func newTestManager(client Client, clock Clock, opts ...Option) *Manager {
	return NewManager(client, Config{MaxSceneBytes: 50 * 1024 * 1024}, clock, opts...)
}

// TestSaveThrottleTable drives the save schedule: dirty at t=0 saves at
// t=0; continuous edits save no earlier than the
// 60s ServerSaveInterval throttle; a quiet room flushes early, at 30s
// since its last PutScene; no edits at all makes no PutFile call.
func TestSaveThrottleTable(t *testing.T) {
	t.Run("no changes makes no PutFile call", func(t *testing.T) {
		client := newFakeClient()
		clock := newFakeClock(time.Unix(0, 0))
		m := newTestManager(client, clock)
		observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))

		m.evaluateAll()

		if got := client.putCount(); got != 0 {
			t.Fatalf("putCount = %d, want 0: nothing was ever posted to save", got)
		}
		// A writable session alone, with no PutScene at all, still
		// earns an eager lock acquisition.
		if got := client.lockCount(); got != 1 {
			t.Fatalf("lockCount = %d, want 1 (eager lock acquisition)", got)
		}
	})

	t.Run("first PutScene saves immediately", func(t *testing.T) {
		client := newFakeClient()
		clock := newFakeClock(time.Unix(0, 0))
		m := newTestManager(client, clock)
		observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))

		if err := m.PutScene(testWopiSrc, []byte("scene-1"), boardapi.SceneMeta{UserID: "alice"}); err != nil {
			t.Fatalf("PutScene() error = %v", err)
		}
		m.evaluateAll()

		if got := client.putCount(); got != 1 {
			t.Fatalf("putCount = %d, want 1 (t=0 first save)", got)
		}
		if got := client.lastPut(); string(got.body) != "scene-1" || got.token != "tok-a" {
			t.Fatalf("last PutFile = %+v, want body scene-1 token tok-a", got)
		}
	})

	t.Run("continuous edits still save at the 60s throttle boundary", func(t *testing.T) {
		client := newFakeClient()
		clock := newFakeClock(time.Unix(0, 0))
		m := newTestManager(client, clock)
		observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))

		mustPutScene(t, m, "scene-0")
		m.evaluateAll()
		if got := client.putCount(); got != 1 {
			t.Fatalf("putCount after first save = %d, want 1", got)
		}

		// The client's own throttle is 10s (hostadapter.ClientSaveInterval);
		// mirror that cadence here so the room never goes quiet. A
		// continuously edited room must still reach the host within 60s, so
		// the 60s ServerSaveInterval throttle must be the only gate, not an
		// idle window that these steady edits would keep pushing out.
		for range 6 {
			clock.Advance(10 * time.Second)
			mustPutScene(t, m, "scene-edit")
			m.evaluateAll()
			if clock.Now().Sub(time.Unix(0, 0)) < hostadapter.ServerSaveInterval {
				if got := client.putCount(); got != 1 {
					t.Fatalf("putCount at t=%s = %d, want still 1 (throttled)", clock.Now(), got)
				}
			}
		}

		// t is now +60s, with an edit landing right on the throttle
		// boundary: the save must fire anyway.
		if got := client.putCount(); got != 2 {
			t.Fatalf("putCount at +60s with continuous edits = %d, want 2: the 60s throttle is the only gate", got)
		}
	})

	t.Run("a quiet room flushes at the 30s idle window, before the 60s cap", func(t *testing.T) {
		client := newFakeClient()
		clock := newFakeClock(time.Unix(0, 0))
		m := newTestManager(client, clock)
		observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))

		mustPutScene(t, m, "scene-0")
		m.evaluateAll()
		if got := client.putCount(); got != 1 {
			t.Fatalf("putCount after first save = %d, want 1", got)
		}

		clock.Advance(5 * time.Second)
		mustPutScene(t, m, "scene-1") // the last edit before the user goes quiet
		m.evaluateAll()
		if got := client.putCount(); got != 1 {
			t.Fatalf("putCount right after the last edit = %d, want still 1", got)
		}

		clock.Advance(idleFlushInterval - time.Second)
		m.evaluateAll()
		if got := client.putCount(); got != 1 {
			t.Fatalf("putCount just before the idle window closes = %d, want still 1", got)
		}

		clock.Advance(time.Second) // now 30s of quiet since the last PutScene, at t=+35s (<60s)
		m.evaluateAll()
		if got := client.putCount(); got != 2 {
			t.Fatalf("putCount after the idle flush = %d, want 2 (the idle-window case)", got)
		}
	})
}

func mustPutScene(t *testing.T, m *Manager, scene string) {
	t.Helper()
	mustPutSceneAs(t, m, testWopiSrc, scene, "alice")
}

// TestTokenLadderFallsBackOnRejection covers the token ladder's choice:
// the most recent writer's token first, and any other tracked,
// unexpired, write-capable token on a 403.
func TestTokenLadderFallsBackOnRejection(t *testing.T) {
	client := newFakeClient()
	client.reject("tok-alice")
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(client, clock)

	observe(m, testWopiSrc, "f-1", "alice", "tok-alice", true, clock.Now().Add(time.Hour))
	observe(m, testWopiSrc, "f-1", "bob", "tok-bob", true, clock.Now().Add(time.Hour))
	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")

	m.evaluateAll()

	if got := client.putCount(); got != 1 {
		t.Fatalf("putCount = %d, want 1", got)
	}
	if got := client.lastPut(); got.token != "tok-bob" {
		t.Fatalf("save token = %q, want the fallback tok-bob", got.token)
	}
}

func mustPutSceneAs(t *testing.T, m *Manager, wopiSrc, scene, userID string) {
	t.Helper()
	if err := m.PutScene(wopiSrc, []byte(scene), boardapi.SceneMeta{UserID: userID}); err != nil {
		t.Fatalf("PutScene() error = %v", err)
	}
}

// TestAllTokensFailBacksOffAndPersistsAfterRoomEmpties covers the case
// where no tracked token works: the Manager keeps the scene and keeps
// retrying with a doubling backoff, and that retry survives the
// last relay member leaving (OnLeave(roomEmpty=true)).
func TestAllTokensFailBacksOffAndPersistsAfterRoomEmpties(t *testing.T) {
	client := newFakeClient()
	client.reject("tok-alice")
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(client, clock)

	observe(m, testWopiSrc, "f-1", "alice", "tok-alice", true, clock.Now().Add(time.Hour))
	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")

	m.evaluateAll() // first attempt fails: backoff = 1s, nextRetryAt = t0+1s
	if got := client.putCount(); got != 0 {
		t.Fatalf("putCount after the failing save = %d, want 0", got)
	}

	m.OnJoin("f-1", "alice", true)
	m.OnLeave("f-1", "alice", true) // roomEmpty: schedules the close grace

	// Still within the 1s backoff: no retry yet.
	m.evaluateAll()
	if got := client.putCount(); got != 0 {
		t.Fatalf("putCount before backoff elapses = %d, want 0", got)
	}

	clock.Advance(time.Second) // backoff elapses, but the close grace (10s) has not
	m.evaluateAll()
	if got := client.putCount(); got != 0 {
		t.Fatalf("putCount = %d, want 0 (still rejected)", got)
	}
	if _, ok := m.GetScene(testWopiSrc); !ok {
		t.Fatal("GetScene = not found, want the scene retained across the failed retries")
	}

	// Advance well past the close grace too: the room must still not be
	// dropped while it is dirty and unsaved, even after the last client
	// leaves.
	clock.Advance(time.Minute)
	m.evaluateAll()
	if _, ok := m.GetScene(testWopiSrc); !ok {
		t.Fatal("GetScene = not found, want the scene retained: a failed close must not discard unsaved work")
	}
}

// TestRoomCloseFlushesAndUnlocksAfterGrace covers the close-grace flow:
// the final flush and unlock fire closeGrace after the last member leaves,
// and a rejoin inside that window cancels the close.
func TestRoomCloseFlushesAndUnlocksAfterGrace(t *testing.T) {
	t.Run("closes after the grace", func(t *testing.T) {
		client := newFakeClient()
		clock := newFakeClock(time.Unix(0, 0))
		m := newTestManager(client, clock)
		observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))
		mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")
		m.evaluateAll()
		if got := client.putCount(); got != 1 {
			t.Fatalf("putCount after the initial save = %d, want 1", got)
		}

		m.OnJoin("f-1", "alice", true)
		m.OnLeave("f-1", "alice", true)

		clock.Advance(closeGrace - time.Second)
		m.evaluateAll()
		if got := client.unlockCount(); got != 0 {
			t.Fatalf("unlockCount before the grace elapses = %d, want 0", got)
		}

		clock.Advance(time.Second)
		m.evaluateAll()
		if got := client.unlockCount(); got != 1 {
			t.Fatalf("unlockCount after the grace = %d, want 1", got)
		}
		if _, ok := m.GetScene(testWopiSrc); ok {
			t.Fatal("GetScene = found, want the room gone after close")
		}
	})

	t.Run("a rejoin within the grace cancels the close", func(t *testing.T) {
		client := newFakeClient()
		clock := newFakeClock(time.Unix(0, 0))
		m := newTestManager(client, clock)
		observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))
		mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")
		m.evaluateAll()

		m.OnJoin("f-1", "alice", true)
		m.OnLeave("f-1", "alice", true)

		clock.Advance(closeGrace / 2)
		m.OnJoin("f-1", "alice", true) // reconnect before the grace elapses

		clock.Advance(closeGrace)
		m.evaluateAll()

		if got := client.unlockCount(); got != 0 {
			t.Fatalf("unlockCount = %d, want 0: the rejoin should have canceled the close", got)
		}
		if _, ok := m.GetScene(testWopiSrc); !ok {
			t.Fatal("GetScene = not found, want the room still open after the canceled close")
		}
	})
}

// TestShutdownFlushesDirtyRooms covers Shutdown making a best-effort
// flush of every dirty room within its deadline.
func TestShutdownFlushesDirtyRooms(t *testing.T) {
	client := newFakeClient()
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(client, clock)
	observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))
	mustPutSceneAs(t, m, testWopiSrc, "scene-1", "alice")

	// No Start(), no evaluateAll(): the room is dirty and has never been
	// saved. Shutdown must flush it on its own.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if got := client.putCount(); got != 1 {
		t.Fatalf("putCount after Shutdown = %d, want 1", got)
	}
	m.mu.Lock()
	dirty := m.rooms[testWopiSrc].dirty
	m.mu.Unlock()
	if dirty {
		t.Fatal("room still dirty after Shutdown flushed it")
	}
}

// TestStartStopBackgroundLoop is a light integration check of Start and
// Shutdown running the real background loop (a short poll interval so
// the test does not depend on the production 2s cadence): a save that
// nothing calls evaluateAll for still lands, driven by the loop's own
// ticker.
func TestStartStopBackgroundLoop(t *testing.T) {
	client := newFakeClient()
	clock := newFakeClock(time.Unix(0, 0))
	m := NewManager(client, Config{}, clock, WithPollInterval(5*time.Millisecond))
	observe(m, testWopiSrc, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))
	m.Start()

	if err := m.PutScene(testWopiSrc, []byte("scene-1"), boardapi.SceneMeta{UserID: "alice"}); err != nil {
		t.Fatalf("PutScene() error = %v", err)
	}

	deadline := time.After(2 * time.Second)
	for client.putCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the background loop to save")
		case <-time.After(time.Millisecond):
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

// TestByFileIDAliasing covers the case where two differently spelled
// WOPISrc values for the same fileID must converge on one roomState, not each get
// their own, competing one that steals the other's m.byFileID entry.
func TestByFileIDAliasing(t *testing.T) {
	const wopiSrc1 = "https://drive.example/wopi/files/f-1"
	const wopiSrc2 = "https://drive.example/wopi/files/f-1?alt=spelling"

	t.Run("two spellings converge on one room", func(t *testing.T) {
		client := newFakeClient()
		clock := newFakeClock(time.Unix(0, 0))
		m := newTestManager(client, clock)

		observe(m, wopiSrc1, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))
		observe(m, wopiSrc2, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))

		m.mu.Lock()
		rs1, rs2 := m.rooms[wopiSrc1], m.rooms[wopiSrc2]
		m.mu.Unlock()
		if rs1 == nil || rs2 == nil || rs1 != rs2 {
			t.Fatalf("m.rooms[wopiSrc1] = %p, m.rooms[wopiSrc2] = %p, want the same room aliased under both spellings", rs1, rs2)
		}

		// A join delivered under the fileID (as the relay always does)
		// must be visible, and later balanced, no matter which spelling's
		// map entry a test happens to read it back through, since both
		// point at the one shared room.
		m.OnJoin("f-1", "alice", true)
		m.mu.Lock()
		live := m.rooms[wopiSrc2].liveUserCount
		m.mu.Unlock()
		if live != 1 {
			t.Fatalf("liveUserCount read via the second spelling = %d, want 1", live)
		}

		m.OnLeave("f-1", "alice", true)
		m.mu.Lock()
		live = m.rooms[wopiSrc1].liveUserCount
		m.mu.Unlock()
		if live != 0 {
			t.Fatalf("liveUserCount read via the first spelling = %d, want 0: the join and the leave shared one counter", live)
		}
	})

	t.Run("closing the room removes every alias key", func(t *testing.T) {
		client := newFakeClient()
		clock := newFakeClock(time.Unix(0, 0))
		m := newTestManager(client, clock)

		observe(m, wopiSrc1, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))
		observe(m, wopiSrc2, "f-1", "alice", "tok-a", true, clock.Now().Add(time.Hour))
		mustPutSceneAs(t, m, wopiSrc1, "scene-1", "alice")
		m.evaluateAll()

		m.OnJoin("f-1", "alice", true)
		m.OnLeave("f-1", "alice", true) // roomEmpty: schedules the close grace

		clock.Advance(closeGrace)
		m.evaluateAll()

		m.mu.Lock()
		_, ok1 := m.rooms[wopiSrc1]
		_, ok2 := m.rooms[wopiSrc2]
		_, okFile := m.byFileID["f-1"]
		m.mu.Unlock()
		if ok1 || ok2 {
			t.Fatalf("m.rooms after close: wopiSrc1 present=%v wopiSrc2 present=%v, want both gone", ok1, ok2)
		}
		if okFile {
			t.Fatal("m.byFileID[f-1] still present after close, want it gone")
		}
	})

	t.Run("the byFileID delete guard leaves a newer room's entry alone", func(t *testing.T) {
		client := newFakeClient()
		clock := newFakeClock(time.Unix(0, 0))
		m := newTestManager(client, clock)

		m.mu.Lock()
		stale := m.roomLocked(wopiSrc1, "f-1")
		// Simulate a fresh room having already taken over the fileID
		// index entry (e.g. gcRoom read its snapshot before a concurrent
		// close-and-reopen ran) before stale's own delete runs.
		fresh := &roomState{wopiSrc: wopiSrc2, wopiSrcs: map[string]struct{}{wopiSrc2: {}}, fileID: "f-1"}
		m.byFileID["f-1"] = fresh
		m.deleteRoomLocked(stale)
		cur, ok := m.byFileID["f-1"]
		m.mu.Unlock()

		if !ok || cur != fresh {
			t.Fatalf("m.byFileID[f-1] = %v (present=%v), want it still pointing at the newer room, untouched by the stale room's delete", cur, ok)
		}
	})
}
