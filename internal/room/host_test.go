package room

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
	"github.com/zeylos/excalidraw-wopi/internal/wopitest"
)

const (
	hostBasePath = "/wopi/files"
	hostOrigin   = "http://fakehost.invalid"
	hostFileID   = "f-1"
)

// localTransport runs a request straight through handler in the calling
// goroutine, the same in-process RoundTripper pattern
// internal/wopitest/host_test.go uses: this repo's sandbox refuses
// ephemeral-port listeners, so a live httptest.Server is not an option
// here.
type localTransport struct{ handler http.Handler }

func (t localTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// hostFixture wires a wopitest.Host and a real wopiclient.Client against
// it, so the lock lifecycle tests below exercise the Manager against
// Drive's actual lock and version-marker semantics, not a hand-rolled
// approximation of them.
type hostFixture struct {
	host   *wopitest.Host
	client *wopiclient.Client
	src    string
}

func newHostFixture(t *testing.T, lockTTL time.Duration) hostFixture {
	t.Helper()
	host := wopitest.New(hostBasePath, lockTTL)
	host.AddUser(wopitest.User{ID: "alice", Name: "Alice", CanWrite: true})
	host.AddUser(wopitest.User{ID: "bob", Name: "Bob", CanWrite: true})
	host.AddFile(hostFileID, hostFileID+".excalidraw", "alice", nil)

	httpClient := &http.Client{Transport: localTransport{handler: host.Handler()}}
	client := wopiclient.New(httpClient, nil, hostadapter.NewDrive())
	return hostFixture{host: host, client: client, src: hostOrigin + hostBasePath + "/" + hostFileID}
}

// TestFirstSaveLocksThenRefreshHolds covers lock acquisition: the first
// save LOCKs before it PUTs, and a refresh LockRefreshInterval later,
// while the lock is still valid, simply extends it.
func TestFirstSaveLocksThenRefreshHolds(t *testing.T) {
	f := newHostFixture(t, time.Hour)
	token := f.host.MintToken("alice", hostFileID)
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(f.client, clock)

	observe(m, f.src, hostFileID, "alice", token, true, clock.Now().Add(24*time.Hour))
	mustPutSceneAs(t, m, f.src, "scene-1", "alice")
	m.evaluateAll()

	stats, ok := f.host.Stats(hostFileID)
	if !ok || stats.PutCount != 1 {
		t.Fatalf("host stats = %+v, ok=%v; want one PutFile", stats, ok)
	}
	lock, err := f.client.GetLock(context.Background(), f.src, token)
	if err != nil {
		t.Fatalf("GetLock() error = %v", err)
	}
	if want := lockValueFor(hostFileID); lock != want {
		t.Fatalf("lock = %q, want %q", lock, want)
	}

	clock.Advance(hostadapter.LockRefreshInterval)
	m.evaluateAll()

	lock, err = f.client.GetLock(context.Background(), f.src, token)
	if err != nil {
		t.Fatalf("GetLock() after refresh error = %v", err)
	}
	if want := lockValueFor(hostFileID); lock != want {
		t.Fatalf("lock after refresh = %q, want %q (still ours)", lock, want)
	}
}

// TestExpiredLockOnRefreshReLocks covers the empty-409 edge: when the
// host's lock has actually expired, a REFRESH_LOCK 409s with an empty
// X-WOPI-Lock, and the Manager must re-LOCK rather than retry the
// refresh, since cache.touch cannot revive an expired lock.
func TestExpiredLockOnRefreshReLocks(t *testing.T) {
	const realLockTTL = 20 * time.Millisecond
	f := newHostFixture(t, realLockTTL)
	token := f.host.MintToken("alice", hostFileID)
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(f.client, clock)

	observe(m, f.src, hostFileID, "alice", token, true, clock.Now().Add(24*time.Hour))
	mustPutSceneAs(t, m, f.src, "scene-1", "alice")
	m.evaluateAll()

	// Let the host's real-time lock TTL lapse. The fake Clock the
	// Manager reads from is unaffected: only Host's own wall-clock TTL
	// needs to pass here, and realLockTTL is short enough that a plain
	// sleep is cheap and not flaky.
	time.Sleep(5 * realLockTTL)

	clock.Advance(hostadapter.LockRefreshInterval)
	m.evaluateAll()

	m.mu.Lock()
	rs := m.rooms[f.src]
	conflict := rs.conflict
	haveLock := rs.haveLock
	m.mu.Unlock()
	if conflict {
		t.Fatal("conflict = true, want the expired-lock edge to re-LOCK cleanly, not to enter conflict state")
	}
	if !haveLock {
		t.Fatal("haveLock = false, want the re-LOCK to have succeeded")
	}

	lock, err := f.client.GetLock(context.Background(), f.src, token)
	if err != nil {
		t.Fatalf("GetLock() error = %v", err)
	}
	if want := lockValueFor(hostFileID); lock != want {
		t.Fatalf("lock after re-LOCK = %q, want %q", lock, want)
	}
}

// TestForeignLockEntersConflict covers the lock-conflict case: a 409
// that carries someone else's lock value pauses saves for the room
// instead of retrying with a different token, and ResolveConflict's
// reload branch clears that state.
func TestForeignLockEntersConflict(t *testing.T) {
	f := newHostFixture(t, time.Hour)
	aliceToken := f.host.MintToken("alice", hostFileID)
	bobToken := f.host.MintToken("bob", hostFileID)
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(f.client, clock)

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
	if stats, _ := f.host.Stats(hostFileID); stats.PutCount != 0 {
		t.Fatalf("PutCount = %d, want 0: a conflicted room must not save", stats.PutCount)
	}

	if err := m.ResolveConflict(f.src, false); err != nil {
		t.Fatalf("ResolveConflict(reload) error = %v", err)
	}
	if m.Conflict(f.src) {
		t.Fatal("Conflict() = true after ResolveConflict(reload)")
	}
	if _, ok := m.GetScene(f.src); ok {
		t.Fatal("GetScene() found a scene, want it dropped so the caller proxies fresh content")
	}
	// TestResolveConflictReloadTakesOverTheForeignLock below covers what
	// happens on the next pass: the foreign lock is taken over rather
	// than left to 409 again.
}

// TestResolveConflictOverwriteForcesASaveOnceTheForeignLockClears covers
// the overwrite path of conflict resolution: it retains a PutScene that
// arrives mid-conflict without saving it, then forces that retained
// scene to the host once the foreign lock that caused the conflict is
// gone. It runs against its own fresh conflict, independent of
// TestForeignLockEntersConflict's reload sub-test.
func TestResolveConflictOverwriteForcesASaveOnceTheForeignLockClears(t *testing.T) {
	f := newHostFixture(t, time.Hour)
	aliceToken := f.host.MintToken("alice", hostFileID)
	bobToken := f.host.MintToken("bob", hostFileID)
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(f.client, clock)

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

	// A PutScene arriving while the room stays conflicted is retained in
	// memory for a later overwrite, but must not reach the host.
	mustPutSceneAs(t, m, f.src, "scene-2", "alice")
	m.evaluateAll()
	if stats, _ := f.host.Stats(hostFileID); stats.PutCount != 0 {
		t.Fatalf("PutCount = %d, want 0: a PutScene arriving mid-conflict must not save to the host", stats.PutCount)
	}

	if err := f.client.Unlock(context.Background(), f.src, bobToken, foreignLock); err != nil {
		t.Fatalf("release the simulated foreign lock: Unlock() error = %v", err)
	}

	if err := m.ResolveConflict(f.src, true); err != nil {
		t.Fatalf("ResolveConflict(overwrite) error = %v", err)
	}
	m.evaluateAll()

	if m.Conflict(f.src) {
		t.Fatal("Conflict() = true after ResolveConflict(overwrite) with the foreign lock cleared")
	}
	stats, ok := f.host.Stats(hostFileID)
	if !ok || stats.PutCount != 1 {
		t.Fatalf("host stats = %+v, ok=%v; want one PutFile after overwrite", stats, ok)
	}
}

// TestResolveConflictReloadTakesOverTheForeignLock covers a reload
// resolution (overwrite=false): it must not leave the foreign lock live
// at the host, or the very next background pass 409s on it again and
// the conflict banner re-arms seconds after the user clicked Reload.
func TestResolveConflictReloadTakesOverTheForeignLock(t *testing.T) {
	f := newHostFixture(t, time.Hour)
	aliceToken := f.host.MintToken("alice", hostFileID)
	bobToken := f.host.MintToken("bob", hostFileID)
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(f.client, clock)

	const foreignLock = "some-other-editor-lock"
	if err := f.client.Lock(context.Background(), f.src, bobToken, foreignLock); err != nil {
		t.Fatalf("simulate a foreign lock: Lock() error = %v", err)
	}

	observe(m, f.src, hostFileID, "alice", aliceToken, true, clock.Now().Add(24*time.Hour))
	mustPutSceneAs(t, m, f.src, "scene-1", "alice")
	m.evaluateAll()
	if !m.Conflict(f.src) {
		t.Fatal("Conflict() = false, want true after the foreign lock 409")
	}

	if err := m.ResolveConflict(f.src, false); err != nil {
		t.Fatalf("ResolveConflict(reload) error = %v", err)
	}
	if m.Conflict(f.src) {
		t.Fatal("Conflict() = true right after ResolveConflict(reload)")
	}

	// Bob never released the foreign lock. The next pass's eager lockDue
	// re-acquisition must take it over via UnlockAndRelock instead of
	// 409ing on it and re-entering conflict.
	m.evaluateAll()

	if m.Conflict(f.src) {
		t.Fatal("Conflict() = true after the next pass, want the foreign lock taken over instead of re-arming the conflict banner")
	}
	lock, err := f.client.GetLock(context.Background(), f.src, aliceToken)
	if err != nil {
		t.Fatalf("GetLock() error = %v", err)
	}
	if want := lockValueFor(hostFileID); lock != want {
		t.Fatalf("lock after reload's takeover = %q, want %q (this room's own value)", lock, want)
	}
}

// TestVersionDriftOnRefreshEntersConflict covers a version drift the
// Manager did not cause (an edit made through some other path while
// this Manager still holds the lock): it is caught on the next lock
// refresh's CheckFileInfo comparison.
func TestVersionDriftOnRefreshEntersConflict(t *testing.T) {
	f := newHostFixture(t, time.Hour)
	token := f.host.MintToken("alice", hostFileID)
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(f.client, clock)

	observe(m, f.src, hostFileID, "alice", token, true, clock.Now().Add(24*time.Hour))
	mustPutSceneAs(t, m, f.src, "scene-1", "alice")
	m.evaluateAll()
	if m.Conflict(f.src) {
		t.Fatal("Conflict() = true after a clean first save")
	}

	// Simulate an edit landing outside this Manager, using the same lock
	// value it holds (a second replica, or a manual admin PutFile).
	if _, err := f.client.PutFile(context.Background(), f.src, token, lockValueFor(hostFileID), []byte("scene-from-elsewhere")); err != nil {
		t.Fatalf("simulate an external PutFile: error = %v", err)
	}

	clock.Advance(hostadapter.LockRefreshInterval)
	m.evaluateAll()

	if !m.Conflict(f.src) {
		t.Fatal("Conflict() = false, want true: the CheckFileInfo on refresh should have caught the version drift")
	}
}

// TestNewUserObserveTriggersVersionCheck covers the other version-check
// trigger: a user joining an already-established room runs the same
// CheckFileInfo comparison, independent of the refresh cadence.
func TestNewUserObserveTriggersVersionCheck(t *testing.T) {
	f := newHostFixture(t, time.Hour)
	aliceToken := f.host.MintToken("alice", hostFileID)
	bobToken := f.host.MintToken("bob", hostFileID)
	clock := newFakeClock(time.Unix(0, 0))
	m := newTestManager(f.client, clock)

	observe(m, f.src, hostFileID, "alice", aliceToken, true, clock.Now().Add(24*time.Hour))
	mustPutSceneAs(t, m, f.src, "scene-1", "alice")
	m.evaluateAll()

	if _, err := f.client.PutFile(context.Background(), f.src, aliceToken, lockValueFor(hostFileID), []byte("scene-from-elsewhere")); err != nil {
		t.Fatalf("simulate an external PutFile: error = %v", err)
	}

	observe(m, f.src, hostFileID, "bob", bobToken, true, clock.Now().Add(24*time.Hour))
	m.evaluateAll()

	if !m.Conflict(f.src) {
		t.Fatal("Conflict() = false, want true: a new user's Observe should have run the version check")
	}
}

// TestEmptyFileRuleAcceptsAnUnlockedPutFile confirms, against the real
// wopitest Host, the fact performSave's unlocked-PutFile fallback relies
// on: an unlocked PutFile succeeds on a file the host still holds no lock
// for and still reports as empty.
// internal/room/save_test.go's TestFirstSaveFallsBackToUnlockedPutFile
// covers the Manager's own fallback code path, which needs a Lock
// failure that is neither a token rejection nor a lock conflict — a
// case Host has no way to produce, since its own LOCK never fails on an
// empty, unlocked file.
func TestEmptyFileRuleAcceptsAnUnlockedPutFile(t *testing.T) {
	f := newHostFixture(t, time.Hour)
	token := f.host.MintToken("alice", hostFileID)

	version, err := f.client.PutFile(context.Background(), f.src, token, "", []byte("scene-1"))
	if err != nil {
		t.Fatalf("unlocked PutFile on an empty, unlocked file: error = %v", err)
	}
	if version == "" {
		t.Fatal("PutFile returned an empty version")
	}

	stats, ok := f.host.Stats(hostFileID)
	if !ok || stats.Size != int64(len("scene-1")) {
		t.Fatalf("host stats = %+v, ok=%v; want the unlocked save to have landed", stats, ok)
	}
}
