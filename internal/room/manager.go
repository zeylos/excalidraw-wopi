// Package room implements the room orchestrator: the per-file save loop
// and the WOPI lock lifecycle. One Manager tracks every open room, keyed
// by WOPISrc (boardapi.RoomStore's key, not the bare file id: see
// boardapi.RoomStore's doc comment). It implements boardapi.RoomStore and
// boardapi.Observer, and it structurally satisfies relay.RoomEvents
// (OnJoin/OnLeave) without importing the relay package.
package room

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/boardapi"
	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/session"
)

const (
	// idleFlushInterval is the "no new PutScene" quiet window that
	// triggers a save even before the ServerSaveInterval throttle would
	// next allow one. It is a service-internal constant, not a WOPI host
	// fact, so it lives here rather than in internal/hostadapter.
	idleFlushInterval = 30 * time.Second

	// initialBackoff and maxBackoff bound the retry schedule after every
	// tracked token fails a save.
	initialBackoff = 1 * time.Second
	maxBackoff     = 5 * time.Minute

	// closeGrace is how long a room lingers, still locked, after its
	// last relay member leaves, so a quick reconnect (a page refresh)
	// does not force a needless unlock and re-lock cycle.
	closeGrace = 10 * time.Second

	// tokenExpiryWarnWindow is how far ahead of a token's expiry the TTL
	// watch fires its warning.
	tokenExpiryWarnWindow = 10 * time.Minute

	// failedTokenTTL is how long a token stays skipped on the save
	// ladder after ErrTokenRejected or ErrNoWriteAccess, before the loop
	// tries it again: long enough that a save pass does not immediately
	// retry a token that just failed, short enough that a one-off
	// rejection does not blacklist it forever.
	failedTokenTTL = 2 * time.Minute

	// saveStalledWindow is how long a room may sit dirty with every save
	// attempt failing before notifyState reports saveStalled.
	saveStalledWindow = 5 * time.Minute

	// gcIdleTimeout is how long a room may sit with nothing live, dirty,
	// or already closing about it before the background loop drops it
	// outright: the safety net for a room that never gets a clean
	// OnLeave (a crashed tab, or one only ever touched over REST, never
	// through the relay), so performClose's own grace-period path never
	// runs for it.
	gcIdleTimeout = 15 * time.Minute

	// stuckConflictLogInterval throttles the operator-visibility log for
	// a conflicted room that stays dirty with no live users: its
	// unsaved scene is retained, never discarded, but an operator should
	// still hear about it periodically rather than never.
	stuckConflictLogInterval = time.Hour

	// evaluateWorkerCount bounds how many rooms the background loop (and
	// Shutdown's flush) evaluates concurrently: enough that one slow
	// room's WOPI calls cannot starve every other
	// room's refresh and save schedule, bounded so a large fleet of rooms
	// cannot open unbounded concurrent connections to the WOPI host.
	evaluateWorkerCount = 4

	// callTimeout bounds one WOPI network call the background loop
	// makes, so a stuck host call cannot wedge the loop forever.
	callTimeout = 30 * time.Second

	// defaultPollInterval is the background loop's wake-up cadence when
	// no event (Observe, PutScene, OnJoin/OnLeave) already woke it. It
	// only needs to be finer than the shortest schedule that has no
	// event to trigger it (the close grace, 10s); the event-driven wake
	// channel handles everything else promptly.
	defaultPollInterval = 2 * time.Second
)

// Config carries the Manager's size caps.
type Config struct {
	// MaxSceneBytes re-enforces boardapi's own PUT size cap on the
	// scene PutScene stores. boardapi already rejects an oversize body
	// before it reaches PutScene; this is defense in depth for a second
	// RoomStore caller that skipped that check, not the primary guard.
	MaxSceneBytes int64
}

// Option configures optional Manager behavior.
type Option func(*Manager)

// WithOnTokenExpiring sets the hook the TTL watch calls, once per token,
// when the best case for that user's token drops under
// tokenExpiryWarnWindow. The client warn/disconnect emit plugs in
// through this hook. fn must return promptly: the background loop calls
// it directly.
func WithOnTokenExpiring(fn func(fileID, userID string)) Option {
	return func(m *Manager) { m.onTokenExpiring = fn }
}

// WithOnConflictChange sets the hook the Manager calls whenever a room's
// client-facing state changes: inConflict on a conflict transition (a foreign lock or a version
// drift entering or clearing), and saveStalled once a dirty room has
// failed every save attempt for at least saveStalledWindow, or
// immediately, once a save pass finds every tracked token has lost write
// access. It fires only on an actual change to either value, so a caller
// wiring this straight to a relay broadcast (internal/app's job) never
// pushes a redundant event. fn must return promptly: the caller calls it
// directly, off m.mu but still on the background loop's own goroutine
// (or, for the resolve side, the calling boardapi request's goroutine).
func WithOnConflictChange(fn func(fileID string, inConflict, saveStalled bool)) Option {
	return func(m *Manager) { m.onConflictChange = fn }
}

// WithOnReloadRequired sets the hook ResolveConflict calls on its reload
// branch (overwrite=false): every client in the room, not just the one
// that resolved it, must reload to pick up the host's current content,
// since the room's retained scene is dropped and every other tab's own
// final-flush-on-unload would otherwise repost it.
// fn must return promptly: ResolveConflict calls it directly.
func WithOnReloadRequired(fn func(fileID string)) Option {
	return func(m *Manager) { m.onReloadRequired = fn }
}

// WithPollInterval overrides the background loop's wake-up cadence.
// Production leaves it at the default; a test that runs Start/Shutdown
// end to end (rather than driving evaluation directly) can shorten it.
func WithPollInterval(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.pollInterval = d
		}
	}
}

// Manager owns every open room's state and drives its save and lock
// lifecycle. Build one with NewManager; call Start to run its background
// loop, and Shutdown to flush every dirty room and stop.
type Manager struct {
	client Client
	cfg    Config
	clock  Clock

	onTokenExpiring  func(fileID, userID string)
	onConflictChange func(fileID string, inConflict, saveStalled bool)
	onReloadRequired func(fileID string)
	pollInterval     time.Duration

	mu       sync.Mutex
	rooms    map[string]*roomState // WOPISrc -> state
	byFileID map[string]*roomState // fileID -> state, for relay's fileID-keyed hooks

	// notifyMu serializes notifyState's compute-then-call-the-hook
	// sequence across every concurrent caller, so two racing callers
	// cannot deliver their onConflictChange broadcasts out of order. It
	// stays off m.mu, since the hook call reaches into
	// the relay and must not run while m.mu is held.
	notifyMu sync.Mutex

	wakeCh   chan struct{}
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewManager builds a Manager. client performs every WOPI call; cfg holds
// the size caps; clock is the time source every schedule reads (pass
// SystemClock in production).
func NewManager(client Client, cfg Config, clock Clock, opts ...Option) *Manager {
	m := &Manager{
		client:       client,
		cfg:          cfg,
		clock:        clock,
		pollInterval: defaultPollInterval,
		rooms:        make(map[string]*roomState),
		byFileID:     make(map[string]*roomState),
		wakeCh:       make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// wake schedules an out-of-cycle background pass without blocking the
// caller: the channel is buffered by one, and a pass that is already
// pending (or already running) coalesces with the new request.
func (m *Manager) wake() {
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
}

// Start runs the background loop on its own goroutine. It is safe to
// call at most once per Manager.
func (m *Manager) Start() {
	m.wg.Add(1)
	go m.loop()
}

func (m *Manager) loop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-m.wakeCh:
			m.evaluateAll()
		case <-ticker.C:
			m.evaluateAll()
		}
	}
}

// Shutdown stops the background loop and makes one best-effort pass to
// flush every dirty room to the WOPI host, bounded by ctx. It ignores the
// normal save throttle and backoff schedule: shutdown gets one direct
// attempt per dirty room, not a wait for its turn.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.stopOnce.Do(func() { close(m.stopCh) })

	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}

	return m.flushAll(ctx)
}

// flushAll makes one best-effort save attempt for every dirty, non-
// conflicted room, across the same bounded worker pool evaluateAll
// uses, with the same per-room inFlight guard: a room whose background
// pass is still running when ctx runs out is skipped rather than saved
// twice concurrently.
func (m *Manager) flushAll(ctx context.Context) error {
	m.mu.Lock()
	rooms := make([]*roomState, 0, len(m.rooms))
	for _, rs := range m.rooms {
		rooms = append(rooms, rs)
	}
	m.mu.Unlock()

	var resultMu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, evaluateWorkerCount)
	var wg sync.WaitGroup

	for _, rs := range rooms {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		default:
		}

		m.mu.Lock()
		dirty := rs.dirty && !rs.conflict
		m.mu.Unlock()
		if !dirty {
			continue
		}
		if !m.tryEnterInFlight(rs) {
			// A background pass for this room is already running;
			// let it finish its own save rather than running a
			// second, concurrent one for the same room.
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(rs *roomState) {
			defer wg.Done()
			defer func() { <-sem }()
			defer m.exitInFlight(rs)

			m.performSave(ctx, rs, m.clock.Now())

			m.mu.Lock()
			stillDirty := rs.dirty
			m.mu.Unlock()
			if stillDirty {
				resultMu.Lock()
				if firstErr == nil {
					firstErr = errShutdownFlushIncomplete
				}
				resultMu.Unlock()
			}
		}(rs)
	}
	wg.Wait()
	return firstErr
}

// evaluateAll runs one background pass over every room, across a bounded
// worker pool: production calls it from loop(); tests call it directly,
// after advancing a fake Clock, to drive the schedule deterministically.
// A room already mid-evaluation (or mid a Shutdown flush) is skipped for
// this pass rather than queued, so a slow WOPI call for one room cannot
// pile up duplicate concurrent attempts for it.
func (m *Manager) evaluateAll() {
	m.mu.Lock()
	rooms := make([]*roomState, 0, len(m.rooms))
	for _, rs := range m.rooms {
		rooms = append(rooms, rs)
	}
	m.mu.Unlock()

	sem := make(chan struct{}, evaluateWorkerCount)
	var wg sync.WaitGroup
	for _, rs := range rooms {
		if !m.tryEnterInFlight(rs) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(rs *roomState) {
			defer wg.Done()
			defer func() { <-sem }()
			defer m.exitInFlight(rs)
			m.evaluateRoom(rs)
		}(rs)
	}
	wg.Wait()
}

// tryEnterInFlight claims rs's inFlight guard, reporting false when
// another goroutine already holds it.
func (m *Manager) tryEnterInFlight(rs *roomState) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rs.inFlight {
		return false
	}
	rs.inFlight = true
	return true
}

func (m *Manager) exitInFlight(rs *roomState) {
	m.mu.Lock()
	rs.inFlight = false
	m.mu.Unlock()
}

// roomLocked finds or creates the room for wopiSrc, and, when fileID is
// known, records it (both on the room and in the fileID index the
// relay's OnJoin/OnLeave hooks resolve through) and (re)derives the
// room's WOPI lock value from it. Keying the lock value on the file id
// rather than the WOPISrc string means two differently spelled WOPISrc
// values for the very same file converge on the same lock value,
// instead of each treating the other's lock as a foreign one.
//
// A wopiSrc spelling never seen before, but whose fileID already has a
// room under a different spelling, aliases onto that same *roomState
// instead of minting a second, competing one: m.byFileID is a plain
// map, so a second room would silently steal the fileID index entry out
// from under the first, leaving that first room permanently
// unreachable from OnJoin/OnLeave (a zombie that never sees its
// liveUserCount drop and keeps refreshing its lock forever). One file
// keeps one room, addressable under several m.rooms keys. The caller
// must hold m.mu.
func (m *Manager) roomLocked(wopiSrc, fileID string) *roomState {
	rs, ok := m.rooms[wopiSrc]
	if !ok {
		if fileID != "" {
			if existing, aliasOK := m.byFileID[fileID]; aliasOK {
				rs = existing
			}
		}
		if rs == nil {
			rs = &roomState{
				wopiSrc:      wopiSrc,
				wopiSrcs:     make(map[string]struct{}),
				tokens:       make(map[string]*tokenInfo),
				failedTokens: make(map[string]time.Time),
			}
		}
		rs.wopiSrcs[wopiSrc] = struct{}{}
		m.rooms[wopiSrc] = rs
	}
	if fileID != "" {
		rs.fileID = fileID
		rs.lockValue = lockValueFor(fileID)
		m.byFileID[fileID] = rs
	}
	return rs
}

// PutScene implements boardapi.RoomStore. It stores the posted scene,
// marks the room dirty, and wakes the background loop; the actual host
// save happens asynchronously, throttled by saveDueLocked's schedule.
func (m *Manager) PutScene(wopiSrc string, data []byte, meta boardapi.SceneMeta) error {
	if m.cfg.MaxSceneBytes > 0 && int64(len(data)) > m.cfg.MaxSceneBytes {
		return errSceneTooLarge
	}
	cp := append([]byte(nil), data...)
	now := m.clock.Now()

	m.mu.Lock()
	rs := m.roomLocked(wopiSrc, "")
	rs.scene = cp
	rs.dirty = true
	rs.sceneSeq++
	rs.lastPutSceneAt = now
	if meta.UserID != "" {
		rs.lastWriterUserID = meta.UserID
	}
	m.mu.Unlock()

	m.wake()
	return nil
}

// GetScene implements boardapi.RoomStore. It returns false both for a
// WOPISrc the Manager has never seen and for one whose retained scene was
// dropped by ResolveConflict's reload branch, so the caller falls back to
// a live WOPI GetFile either way.
func (m *Manager) GetScene(wopiSrc string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rs, ok := m.rooms[wopiSrc]
	if !ok || rs.scene == nil {
		return nil, false
	}
	return append([]byte(nil), rs.scene...), true
}

// Observe implements boardapi.Observer. boardapi calls it on every
// authenticated board API request, so the Manager's token registry
// stays current with every session it could pick a save token from. A
// newly observed user in an already-established room schedules a
// version check: the next background pass compares the host's live
// Version against the last one this Manager posted, to catch a change
// made outside this service's own lock. It makes no network call
// itself: a writable session with no lock yet only flips state here;
// evaluateRoom's own
// lockDue check (save.go) is what actually acquires the lock, on the
// background loop's own goroutine, once wake() below runs a pass.
func (m *Manager) Observe(claims session.Claims) {
	m.mu.Lock()
	rs := m.roomLocked(claims.WOPISrc, claims.FileID)
	_, existed := rs.tokens[claims.UserID]
	rs.tokens[claims.UserID] = &tokenInfo{
		token:     claims.AccessToken,
		expiresAt: claims.ExpiresAt,
		canWrite:  claims.CanWrite,
	}
	now := m.clock.Now()
	m.pruneTokensLocked(rs, now)
	if !existed && rs.haveVersion && !rs.conflict {
		rs.pendingVersionCheck = true
	}
	m.mu.Unlock()

	m.wake()
}

// OnJoin structurally implements relay.RoomEvents. The relay dispatches
// it synchronously, from inside its own per-room emit lock, at the join
// call site (dispatching it via `go` let a leave overtake an earlier
// join in delivery order), so it must stay fast: it only updates
// counters and wakes the background loop, taking only its own mu,
// never calling back into the relay.
func (m *Manager) OnJoin(fileID, userID string, _ bool) {
	m.mu.Lock()
	rs, ok := m.byFileID[fileID]
	if !ok {
		m.mu.Unlock()
		// No prior Observe has told the Manager this file's WOPISrc, so
		// there is no room to attach a join to yet. In the real launch
		// flow the editor's first GET /api/board (which calls Observe)
		// happens before it opens the relay socket, so this is expected
		// only for a socket that connects unusually early; it self-heals
		// on that client's first REST call.
		slog.Debug("room: OnJoin for a file with no known WOPISrc yet", "fileID", fileID, "userID", userID)
		return
	}
	rs.liveUserCount++
	// A rejoin during the close grace cancels the pending close: the
	// room stays locked and open instead of unlocking and immediately
	// needing to re-lock.
	rs.closing = false
	m.mu.Unlock()

	m.wake()
}

// OnLeave structurally implements relay.RoomEvents. See OnJoin's doc
// comment on why it must stay fast and call back into nothing.
func (m *Manager) OnLeave(fileID, _ string, roomEmpty bool) {
	m.mu.Lock()
	rs, ok := m.byFileID[fileID]
	if !ok {
		m.mu.Unlock()
		return
	}
	if rs.liveUserCount > 0 {
		rs.liveUserCount--
	}
	if roomEmpty {
		rs.closing = true
		rs.closeAt = m.clock.Now().Add(closeGrace)
	}
	m.mu.Unlock()

	m.wake()
}

// Conflict reports whether wopiSrc's room is paused on a detected
// conflict.
func (m *Manager) Conflict(wopiSrc string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs, ok := m.rooms[wopiSrc]
	return ok && rs.conflict
}

// SaveStalled reports whether wopiSrc's room has sat dirty with every save
// attempt failing for at least saveStalledWindow, or
// has lost write access on every tracked token. It shares notifyState's
// derivation, so boardapi's GET /api/board/conflict poll answers in the
// same shape as the relay's live conflict-state push.
func (m *Manager) SaveStalled(wopiSrc string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs, ok := m.rooms[wopiSrc]
	if !ok {
		return false
	}
	return saveStalledLocked(rs, m.clock.Now())
}

// ResolveConflict clears wopiSrc's conflict state. overwrite=true forces
// the retained scene to the WOPI host on the next background pass, ignoring
// the normal throttle, with the room's own lock; when a foreign lock is
// still on record, that next pass uses UnlockAndRelock against it and skips
// one version check, since expectedVersion is stale by definition.
// overwrite=false drops the retained scene instead, so the next GetScene
// proxies fresh content from the WOPI host, and every client in the room is
// told to reload. Either branch resumes the save and refresh loop. It is a
// no-op, returning nil, for a wopiSrc with no tracked conflict (including
// one the Manager has never seen).
func (m *Manager) ResolveConflict(wopiSrc string, overwrite bool) error {
	m.mu.Lock()
	rs, ok := m.rooms[wopiSrc]
	if !ok || !rs.conflict {
		m.mu.Unlock()
		return nil
	}

	rs.conflict = false
	foreignLock := rs.conflictForeignLock
	rs.conflictForeignLock = ""
	if overwrite {
		rs.dirty = true
		rs.backoff = 0
		rs.nextRetryAt = time.Time{}
		rs.lastSaveAttemptAt = time.Time{} // bypass the throttle: save now
		rs.skipVersionCheckOnce = true
		if foreignLock != "" {
			rs.forceUnlockValue = foreignLock
		}
	} else {
		rs.scene = nil
		rs.dirty = false
		rs.haveVersion = false
		if foreignLock != "" {
			// Reload means the host's current content wins and this room
			// resumes ownership: without this, the foreign lock stays
			// live at the host, so the next background pass 409s on it
			// again and the conflict banner re-arms seconds after the
			// user clicked Reload. The eager lockDue re-acquisition
			// (evaluateRoom, save.go) runs ensureLocked, whose
			// forceUnlockValue branch takes it over the same way the
			// overwrite branch above already does.
			rs.forceUnlockValue = foreignLock
		}
	}
	fileID := rs.fileID
	reloadHook := m.onReloadRequired
	m.mu.Unlock()

	m.notifyState(rs)
	if !overwrite && reloadHook != nil {
		reloadHook(fileID)
	}

	m.wake()
	return nil
}

// enterConflict marks rs as being in a foreign-lock or version-drift
// conflict. foreignLock, when non-empty, records the value a
// PutFile/LOCK/REFRESH_LOCK 409 reported; dropLock reports whether the
// caller's own WOPI lock is now known stale.
func (m *Manager) enterConflict(rs *roomState, foreignLock string, dropLock bool) {
	m.mu.Lock()
	rs.conflict = true
	if foreignLock != "" {
		rs.conflictForeignLock = foreignLock
	}
	if dropLock {
		rs.haveLock = false
	}
	m.mu.Unlock()

	m.notifyState(rs)
}

// notifyState pushes onConflictChange whenever rs's client-facing state
// (inConflict, saveStalled) actually changed since the last call: a
// conflict entering or clearing, or a dirty room crossing
// saveStalledWindow of consecutive save failures. It dedups against
// rs.lastReportedConflict/lastReportedSaveStalled, so a repeat call
// with unchanged state fires nothing. notifyMu holds the compute step
// and the hook call together, so two concurrent callers cannot
// interleave and deliver their broadcasts out of order.
func (m *Manager) notifyState(rs *roomState) {
	m.notifyMu.Lock()
	defer m.notifyMu.Unlock()

	m.mu.Lock()
	now := m.clock.Now()
	inConflict := rs.conflict
	saveStalled := saveStalledLocked(rs, now)
	changed := inConflict != rs.lastReportedConflict || saveStalled != rs.lastReportedSaveStalled
	if changed {
		rs.lastReportedConflict = inConflict
		rs.lastReportedSaveStalled = saveStalled
	}
	fileID := rs.fileID
	hook := m.onConflictChange
	m.mu.Unlock()

	if changed && hook != nil {
		hook(fileID, inConflict, saveStalled)
	}
}

// saveStalledLocked reports whether rs has sat dirty with every save
// attempt failing for at least saveStalledWindow, or, immediately,
// with every tracked token having lost write access
// (rs.writeLost): a deleted or revoked-write file otherwise leaves the
// client with no signal for the full window. The caller must hold the
// owning Manager's mu.
func saveStalledLocked(rs *roomState, now time.Time) bool {
	windowElapsed := !rs.firstFailedSaveAt.IsZero() && now.Sub(rs.firstFailedSaveAt) >= saveStalledWindow
	return rs.dirty && (rs.writeLost || windowElapsed)
}

// pruneTokensLocked drops every expired token from rs, and, with it,
// every failedTokens entry for a token no longer tracked (a fresh token
// for the same user gets a clean slate on the ladder), plus any
// failedTokens entry whose failedTokenTTL has already elapsed. The
// caller must hold m.mu.
func (m *Manager) pruneTokensLocked(rs *roomState, now time.Time) {
	live := make(map[string]bool, len(rs.tokens))
	for userID, info := range rs.tokens {
		if !now.Before(info.expiresAt) {
			delete(rs.tokens, userID)
			continue
		}
		live[info.token] = true
	}
	for token, failedAt := range rs.failedTokens {
		if !live[token] || now.Sub(failedAt) >= failedTokenTTL {
			delete(rs.failedTokens, token)
		}
	}
}

// roomLiveLocked reports whether rs should keep its lock refreshed while
// a room is live: it has a relay-tracked member, an unexpired boardapi
// session, or unsaved changes. The caller must hold m.mu.
func (m *Manager) roomLiveLocked(rs *roomState, now time.Time) bool {
	if rs.dirty || rs.liveUserCount > 0 {
		return true
	}
	for _, info := range rs.tokens {
		if now.Before(info.expiresAt) {
			return true
		}
	}
	return false
}

// saveDueLocked reports whether rs has a save that should run now: the
// pending backoff retry, the room's very first save, the ServerSaveInterval
// throttle, or the idleFlushInterval quiet window, whichever the current
// state calls for. The caller must hold m.mu.
func (m *Manager) saveDueLocked(rs *roomState, now time.Time) bool {
	if !rs.dirty || rs.conflict {
		return false
	}
	if !rs.nextRetryAt.IsZero() {
		return !now.Before(rs.nextRetryAt)
	}
	if rs.lastSaveAttemptAt.IsZero() {
		return true
	}
	throttleDue := rs.lastSaveAttemptAt.Add(hostadapter.ServerSaveInterval)
	idleDue := rs.lastPutSceneAt.Add(idleFlushInterval)
	due := throttleDue
	if idleDue.Before(due) {
		due = idleDue
	}
	return !now.Before(due)
}

// closeSaveDueLocked reports whether performClose's final flush should run
// now: the last disconnect is exempt from the ServerSaveInterval throttle
// and the idleFlushInterval window, so, unlike saveDueLocked, this drops
// both; a pending backoff wait (rs.nextRetryAt) still paces retries after
// every tracked token failed, the same as it does everywhere else. The
// caller must hold m.mu.
func closeSaveDueLocked(rs *roomState, now time.Time) bool {
	if !rs.dirty || rs.conflict {
		return false
	}
	if !rs.nextRetryAt.IsZero() {
		return !now.Before(rs.nextRetryAt)
	}
	return true
}
