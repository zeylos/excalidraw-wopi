package room

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/hostadapter"
	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

// passPlan holds evaluateRoom's per-pass decisions, computed once under
// m.mu by planPass, plus the roomState fields the dispatch below still
// needs after the unlock.
type passPlan struct {
	versionCheckDue bool
	closeDue        bool
	lockDue         bool
	gcDue           bool
	needRefresh     bool
	logStuck        bool
	haveLock        bool
	lockValue       string
	wopiSrc         string
	fileID          string
}

// planPass computes evaluateRoom's due-ness for rs under m.mu. It must
// never make a WOPI call itself, so evaluateRoom's dispatch below is free
// to run every call outside the lock.
func (m *Manager) planPass(rs *roomState, now time.Time) passPlan {
	m.mu.Lock()
	m.pruneTokensLocked(rs, now)
	live := m.roomLiveLocked(rs, now)
	closeDue := rs.closing && !now.Before(rs.closeAt)
	versionCheckDue, needRefresh := m.versionCheckPlanLocked(rs, now, live, closeDue)
	// Acquire a lock as soon as a writable session is known, instead of
	// waiting for the room's first save to come due (up to
	// ServerSaveInterval, or forever for a room nobody ever dirties). No
	// network call happens here; this only decides whether evaluateRoom's
	// own steps below should try.
	lockDue := !rs.haveLock && !rs.conflict && !closeDue && len(rs.tokenLadder(now)) > 0
	gcDue := m.idleGCPlanLocked(rs, now, live)
	logStuck := m.stuckConflictPlanLocked(rs, now)

	plan := passPlan{
		versionCheckDue: versionCheckDue,
		closeDue:        closeDue,
		lockDue:         lockDue,
		gcDue:           gcDue,
		needRefresh:     needRefresh,
		logStuck:        logStuck,
		haveLock:        rs.haveLock,
		lockValue:       rs.lockValue,
		wopiSrc:         rs.wopiSrc,
		fileID:          rs.fileID,
	}
	m.mu.Unlock()
	return plan
}

// versionCheckPlanLocked decides whether this pass must run checkVersion,
// and whether the lock is due for its periodic REFRESH_LOCK. The caller
// must hold m.mu.
func (m *Manager) versionCheckPlanLocked(rs *roomState, now time.Time, live, closeDue bool) (versionCheckDue, needRefresh bool) {
	versionCheckDue = rs.pendingVersionCheck
	needRefresh = rs.haveLock && !rs.conflict && live && now.Sub(rs.lastLockAt) >= hostadapter.LockRefreshInterval
	if needRefresh {
		versionCheckDue = true // A lock refresh also runs a version check.
	}
	if rs.skipVersionCheckOnce {
		// expectedVersion is stale by definition right after a forced
		// overwrite: skip the one check that would otherwise
		// spuriously re-enter conflict on the version this save is about
		// to replace.
		versionCheckDue = false
		rs.skipVersionCheckOnce = false
	}
	// Clear pendingVersionCheck only on the path below that actually runs
	// checkVersion for it: a room that is closeDue this pass skips the
	// check entirely (see below), so a pending check must survive to a
	// later pass instead of being silently dropped here.
	if versionCheckDue && !closeDue {
		rs.pendingVersionCheck = false
	}
	return versionCheckDue, needRefresh
}

// idleGCPlanLocked tracks how long rs has had nothing live, dirty, or
// closing about it, so the sweep below can GC a room that never gets a
// clean OnLeave. The caller must hold m.mu.
func (m *Manager) idleGCPlanLocked(rs *roomState, now time.Time, live bool) bool {
	idle := !live && !rs.dirty && !rs.closing
	if idle {
		if rs.idleSince.IsZero() {
			rs.idleSince = now
		}
	} else {
		rs.idleSince = time.Time{}
	}
	return idle && now.Sub(rs.idleSince) >= gcIdleTimeout
}

// stuckConflictPlanLocked tracks how long a conflicted room has sat dirty
// with no live users, for the once-per-hour visibility log. The caller
// must hold m.mu.
func (m *Manager) stuckConflictPlanLocked(rs *roomState, now time.Time) bool {
	stuckConflict := rs.conflict && rs.dirty && rs.liveUserCount == 0
	logStuck := stuckConflict && (rs.lastStuckLogAt.IsZero() || now.Sub(rs.lastStuckLogAt) >= stuckConflictLogInterval)
	if logStuck {
		rs.lastStuckLogAt = now
	}
	return logStuck
}

// evaluateRoom is the background loop's per-room decision point. It reads
// rs's due-ness under m.mu, releases the lock before making any WOPI
// call, and re-checks state that a concurrent Observe/PutScene/OnJoin/
// OnLeave call, or an earlier step in this same pass, could have changed.
func (m *Manager) evaluateRoom(rs *roomState) {
	now := m.clock.Now()

	plan := m.planPass(rs, now)

	if plan.logStuck {
		slog.Warn("room: conflicted room stuck dirty with no live users; retaining the unsaved scene",
			"wopiSrc", plan.wopiSrc, "fileID", plan.fileID)
	}

	// Both must run every pass, including one that takes an early return
	// below: a closing or about-to-be-GC'd room should still warn about
	// an expiring token and still push a client-facing state change, not
	// go silent on its way out.
	m.checkTokenExpiry(rs, now)
	defer m.notifyState(rs)

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	if plan.gcDue {
		m.gcRoom(ctx, rs, plan.haveLock, plan.lockValue)
		return
	}

	if plan.versionCheckDue && !plan.closeDue {
		m.checkVersion(ctx, rs, now)
	}

	if plan.closeDue {
		m.performClose(ctx, rs, now)
		return
	}

	if plan.needRefresh {
		m.mu.Lock()
		stillOK := !rs.conflict
		m.mu.Unlock()
		if stillOK {
			m.refreshLock(ctx, rs, now)
		}
	} else if plan.lockDue {
		m.mu.Lock()
		ladder := rs.tokenLadder(now)
		m.mu.Unlock()
		if len(ladder) > 0 {
			if err := m.ensureLocked(ctx, rs, ladder[0].token, now); err != nil {
				slog.Warn("room: eager lock acquisition failed", "wopiSrc", plan.wopiSrc, "error", err)
			}
		}
	}

	m.mu.Lock()
	saveDue := m.saveDueLocked(rs, now)
	m.mu.Unlock()
	if saveDue {
		m.performSave(ctx, rs, now)
	}
}

// gcRoom drops a room that has sat idle (not live, not dirty, no close
// already in flight) for at least gcIdleTimeout: the safety net for a
// room that never gets a clean OnLeave, so performClose's own
// grace-period path never runs for it. Any held lock is released first,
// best effort; a failure leaves the room in place to retry on the next
// pass rather than dropping its map entry with a stale lock still held.
func (m *Manager) gcRoom(ctx context.Context, rs *roomState, haveLock bool, lockValue string) {
	if haveLock {
		m.mu.Lock()
		ladder := rs.tokenLadder(m.clock.Now())
		m.mu.Unlock()
		if len(ladder) > 0 {
			if err := m.client.Unlock(ctx, rs.wopiSrc, ladder[0].token, lockValue); err != nil {
				slog.Warn("room: unlock during idle GC failed; will retry", "wopiSrc", rs.wopiSrc, "error", err)
				return
			}
			// A revived room (the stillIdle re-check below finds it live
			// again) must not believe it still holds a lock the host no
			// longer has, or the eager lockDue re-acquire never fires
			// (mirrors performClose's own haveLock reset).
			m.mu.Lock()
			rs.haveLock = false
			m.mu.Unlock()
		} else {
			slog.Warn("room: idle GC found no usable token to unlock; leaving the lock for the host's TTL to expire",
				"wopiSrc", rs.wopiSrc)
		}
	}

	m.mu.Lock()
	// A concurrent Observe/PutScene/OnJoin could have revived the room
	// between the check that scheduled this GC and this delete; re-verify
	// under the lock rather than dropping a room that came back to life.
	stillIdle := !m.roomLiveLocked(rs, m.clock.Now()) && !rs.dirty && !rs.closing
	if stillIdle {
		m.deleteRoomLocked(rs)
	} else {
		rs.idleSince = time.Time{}
	}
	m.mu.Unlock()
}

// performSave runs one save attempt for rs: it always locks before it
// saves (the unlocked path below runs only as a documented fallback),
// walks the token ladder on a rejection, and records the outcome. It is
// also Shutdown's flush path, called directly and unconditionally on
// every dirty room.
func (m *Manager) performSave(ctx context.Context, rs *roomState, now time.Time) {
	m.mu.Lock()
	if !rs.dirty || rs.conflict {
		m.mu.Unlock()
		return
	}
	scene := rs.scene
	seq := rs.sceneSeq
	lockValue := rs.lockValue
	ladder := rs.tokenLadder(now)
	// firstSaveDone is a one-shot latch, set here regardless of how this
	// attempt turns out, so the unlocked-PutFile fallback below can never
	// re-arm on a later save: lastSaveAttemptAt is not safe for this,
	// since ResolveConflict resets it to bypass the throttle on an
	// overwrite.
	firstEverAttempt := !rs.firstSaveDone
	rs.firstSaveDone = true
	m.mu.Unlock()

	if len(ladder) == 0 {
		// An empty ladder is not itself proof every token lost write access
		// (a token can also be missing or expired); it must not clear a
		// writeLost flag an earlier pass already set.
		m.recordSaveFailure(rs, now, errNoUsableToken, false)
		return
	}

	var lastErr error
	// allNoWrite is true only while every failure in this pass is ErrNoWriteAccess.
	allNoWrite := true
	for _, cand := range ladder {
		res := m.attemptSave(ctx, rs, cand, now, scene, seq, lockValue, firstEverAttempt)
		switch res.outcome {
		case saveOutcomeDone, saveOutcomeConflictHandled:
			return
		case saveOutcomeTryNext:
			lastErr = res.err
			if !res.noWrite {
				allNoWrite = false
			}
		}
	}

	m.recordSaveFailure(rs, now, lastErr, allNoWrite)
}

// saveAttemptOutcome classifies one performSave ladder candidate: whether
// the save landed, a conflict already ended the whole pass, or the loop
// should carry the error on to the next candidate.
type saveAttemptOutcome int

const (
	saveOutcomeDone saveAttemptOutcome = iota
	saveOutcomeConflictHandled
	saveOutcomeTryNext
)

// saveAttempt is attemptSave's result for one ladder candidate. err and
// noWrite are meaningful only for saveOutcomeTryNext: err is the failure
// to carry as performSave's lastErr, and noWrite reports whether it was
// ErrNoWriteAccess, for performSave's allNoWrite tracking.
type saveAttempt struct {
	outcome saveAttemptOutcome
	err     error
	noWrite bool
}

// attemptSave runs one performSave ladder candidate: ensureLocked, then
// PutFile. It classifies both calls' failures so performSave's loop can
// stay a plain dispatch over the three outcomes.
func (m *Manager) attemptSave(ctx context.Context, rs *roomState, cand tokenCandidate, now time.Time, scene []byte, seq uint64, lockValue string, firstEverAttempt bool) saveAttempt {
	if err := m.ensureLocked(ctx, rs, cand.token, now); err != nil {
		res := m.classifyTokenAttempt(rs, now, cand.token, err)
		switch {
		case res.rejected:
			return saveAttempt{outcome: saveOutcomeTryNext, err: err}
		case res.noWrite:
			// A 401 means the token is valid but the write bit is off;
			// treat it like a rejection, not the generic transient-fault
			// path below.
			return saveAttempt{outcome: saveOutcomeTryNext, err: err, noWrite: true}
		case res.conflict != nil:
			// ensureLocked already flipped rs.conflict for a foreign
			// lock; no other token changes that outcome.
			return saveAttempt{outcome: saveOutcomeConflictHandled}
		}

		// Some other failure (network, host fault). An unlocked
		// PutFile succeeds only while the host still holds no lock and
		// the file is still empty. Try it as a last resort on the
		// room's very first ever save, so a host quirk in Lock itself
		// cannot block the first write to a brand new file that a
		// plain, unlocked PutFile would have accepted.
		if firstEverAttempt {
			if version, putErr := m.client.PutFile(ctx, rs.wopiSrc, cand.token, "", scene); putErr == nil {
				m.recordSaveSuccess(rs, now, version, seq)
				return saveAttempt{outcome: saveOutcomeDone}
			}
		}
		return saveAttempt{outcome: saveOutcomeTryNext, err: err}
	}

	version, err := m.client.PutFile(ctx, rs.wopiSrc, cand.token, lockValue, scene)
	if err == nil {
		m.recordSaveSuccess(rs, now, version, seq)
		return saveAttempt{outcome: saveOutcomeDone}
	}

	res := m.classifyTokenAttempt(rs, now, cand.token, err)
	switch {
	case res.rejected:
		return saveAttempt{outcome: saveOutcomeTryNext, err: err}
	case res.noWrite:
		return saveAttempt{outcome: saveOutcomeTryNext, err: err, noWrite: true}
	case res.conflict != nil:
		if res.conflict.CurrentLock != "" && res.conflict.CurrentLock != lockValue {
			m.enterConflict(rs, res.conflict.CurrentLock, true)
			return saveAttempt{outcome: saveOutcomeConflictHandled}
		}
		// An empty or matching lock on a PutFile 409 straight after a
		// successful Lock is unexpected; treat it as transient and
		// let the next pass re-lock and retry.
		m.mu.Lock()
		rs.haveLock = false
		m.mu.Unlock()
		return saveAttempt{outcome: saveOutcomeTryNext, err: err}
	default:
		return saveAttempt{outcome: saveOutcomeTryNext, err: err}
	}
}

// ensureLocked runs the LOCK state machine: a success or a 409 that
// already carries our own value both count as holding the lock; an
// empty-lock 409 (the expired/absent edge) gets one LOCK retry; any
// foreign value enters conflict state.
//
// When ResolveConflict(overwrite) left a forceUnlockValue on record, this
// tries UnlockAndRelock against it first: a plain LOCK never succeeds
// while the host still holds someone else's value, so an overwrite could
// otherwise never actually land while the foreign lock persists.
// forceUnlockValue is consumed here as a one-shot, regardless of the
// outcome.
func (m *Manager) ensureLocked(ctx context.Context, rs *roomState, token string, now time.Time) error {
	m.mu.Lock()
	lockValue := rs.lockValue
	forceUnlock := rs.forceUnlockValue
	rs.forceUnlockValue = ""
	m.mu.Unlock()

	if forceUnlock != "" {
		err := m.client.UnlockAndRelock(ctx, rs.wopiSrc, token, lockValue, forceUnlock)
		if err == nil {
			m.markLocked(rs, now)
			return nil
		}
		if conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err); ok {
			if conflict.CurrentLock != "" && conflict.CurrentLock != lockValue {
				m.enterConflict(rs, conflict.CurrentLock, true)
				return err
			}
			// An empty CurrentLock (the old lock had already expired) or a
			// 409 that already carries our own value: fall through to a
			// plain LOCK below, which succeeds either way (a same-value
			// refresh, or a fresh acquire once nothing else holds the
			// file).
		} else {
			// Not a lock conflict at all: this candidate's own token is the
			// problem (rejected, no write access, or a generic host
			// fault), not the foreign lock UnlockAndRelock was aimed at.
			// Restore the one-shot Overwrite intent so performSave's next
			// ladder candidate still gets to retry UnlockAndRelock against
			// it, instead of falling through to a plain LOCK that would
			// 409 on the still-live foreign lock and re-enter conflict
			// (a token-only failure must not spend the user's Overwrite
			// decision).
			m.mu.Lock()
			rs.forceUnlockValue = forceUnlock
			m.mu.Unlock()
			return err
		}
	}

	err := m.client.Lock(ctx, rs.wopiSrc, token, lockValue)
	if err == nil {
		m.markLocked(rs, now)
		return nil
	}

	conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err)
	if !ok {
		return err
	}

	if conflict.CurrentLock == "" {
		err2 := m.client.Lock(ctx, rs.wopiSrc, token, lockValue)
		if err2 == nil {
			m.markLocked(rs, now)
			return nil
		}
		// A foreign lock discovered on this retry (own-value or genuinely
		// foreign) must resolve exactly like the first attempt's own
		// default case below: a bare `return err2` here would leave the
		// room retrying LOCK+PUT against the host every poll interval
		// with rs.conflict never set, and no conflict-state event ever
		// reaching clients.
		if conflict2, ok := errors.AsType[wopiclient.ErrLockConflict](err2); ok && conflict2.CurrentLock != "" {
			return m.resolveForeignLock(rs, lockValue, conflict2, err2, now)
		}
		return err2
	}

	return m.resolveForeignLock(rs, lockValue, conflict, err, now)
}

// resolveForeignLock handles a LOCK 409 whose CurrentLock is not the
// empty-lock retry edge. This room's own value counts as already holding
// it (WOPI defines a same-value LOCK as a refresh). Any other value enters
// conflict state. origErr is returned unchanged when the value does not
// resolve the lock.
func (m *Manager) resolveForeignLock(rs *roomState, lockValue string, conflict wopiclient.ErrLockConflict, origErr error, now time.Time) error {
	if conflict.CurrentLock == lockValue {
		m.markLocked(rs, now)
		return nil
	}
	m.enterConflict(rs, conflict.CurrentLock, true)
	return origErr
}

func (m *Manager) markLocked(rs *roomState, now time.Time) {
	m.mu.Lock()
	rs.haveLock = true
	rs.lastLockAt = now
	m.mu.Unlock()
}

// refreshLock tries REFRESH_LOCK down the token ladder; an empty-lock 409
// means the lock already expired (cache.touch cannot revive it), so that
// candidate falls through to a full re-LOCK instead; a foreign value
// enters conflict state. Like performSave, it tracks whether every
// candidate failure in the pass is ErrNoWriteAccess, so a live room that
// loses write access on refresh reports saveStalled at once, on the pass
// that next finds it dirty, rather than after saveStalledWindow.
func (m *Manager) refreshLock(ctx context.Context, rs *roomState, now time.Time) {
	m.mu.Lock()
	ladder := rs.tokenLadder(now)
	lockValue := rs.lockValue
	m.mu.Unlock()
	if len(ladder) == 0 {
		return
	}

	allNoWrite := true
	for _, cand := range ladder {
		err := m.client.RefreshLock(ctx, rs.wopiSrc, cand.token, lockValue)
		if err == nil {
			m.markLocked(rs, now)
			return
		}

		res := m.classifyTokenAttempt(rs, now, cand.token, err)
		switch {
		case res.conflict != nil:
			switch res.conflict.CurrentLock {
			case "":
				if lockErr := m.ensureLocked(ctx, rs, cand.token, now); lockErr == nil {
					return
				}
				allNoWrite = false
				continue
			case lockValue:
				m.markLocked(rs, now)
				return
			default:
				m.enterConflict(rs, res.conflict.CurrentLock, true)
				return
			}
		case res.rejected:
			allNoWrite = false
			continue
		case res.noWrite:
			continue
		default:
			allNoWrite = false
			slog.Warn("room: refresh lock failed", "wopiSrc", rs.wopiSrc, "error", err)
		}
	}

	if allNoWrite {
		m.mu.Lock()
		rs.writeLost = true
		m.mu.Unlock()
	}
}

// checkVersion compares the host's live Version against the last one this
// Manager posted, and enters conflict state on a mismatch. It is a no-op
// until the room has posted at least one save (haveVersion), since there
// is nothing yet to compare against.
func (m *Manager) checkVersion(ctx context.Context, rs *roomState, now time.Time) {
	m.mu.Lock()
	token := rs.anyLiveToken(now)
	expected := rs.expectedVersion
	haveVersion := rs.haveVersion
	m.mu.Unlock()

	if token == "" || !haveVersion {
		return
	}

	info, err := m.client.CheckFileInfo(ctx, rs.wopiSrc, token)
	if err != nil {
		slog.Warn("room: version check failed", "wopiSrc", rs.wopiSrc, "error", err)
		return
	}
	if info.Version != expected {
		m.enterConflict(rs, "", false)
		slog.Warn("room: version drift detected, entering conflict state",
			"wopiSrc", rs.wopiSrc, "expected", expected, "got", info.Version)
	}
}

// performClose runs the final flush and unlock. The last disconnect is
// exempt from the ServerSaveInterval/idle-flush throttle, so this save
// runs as soon as the close grace elapses, gated only by a still-pending
// backoff wait (closeSaveDueLocked). A dirty room that still fails to
// save (every tracked token rejected) stays open, still closing, and
// retries on a later pass: a save keeps retrying even after the last
// client leaves, so this never discards the retained scene.
func (m *Manager) performClose(ctx context.Context, rs *roomState, now time.Time) {
	m.mu.Lock()
	if !rs.closing || now.Before(rs.closeAt) {
		m.mu.Unlock()
		return
	}
	dirty := rs.dirty
	conflict := rs.conflict
	due := closeSaveDueLocked(rs, now)
	m.mu.Unlock()

	if dirty && !conflict {
		if !due {
			return
		}
		m.performSave(ctx, rs, now)
	}

	m.mu.Lock()
	stillClosing := rs.closing && !now.Before(rs.closeAt)
	stillDirty := rs.dirty
	haveLock := rs.haveLock
	lockValue := rs.lockValue
	m.mu.Unlock()

	if !stillClosing {
		return // a rejoin landed mid-save and canceled the close
	}
	if stillDirty {
		return // the save failed; keep the room open and retry later
	}

	if haveLock {
		m.mu.Lock()
		ladder := rs.tokenLadder(now)
		m.mu.Unlock()
		if len(ladder) > 0 {
			if err := m.client.Unlock(ctx, rs.wopiSrc, ladder[0].token, lockValue); err != nil {
				slog.Warn("room: unlock on close failed", "wopiSrc", rs.wopiSrc, "error", err)
			} else {
				m.mu.Lock()
				rs.haveLock = false
				m.mu.Unlock()
			}
		} else {
			slog.Warn("room: no usable token to unlock on close; leaving the lock for the host's TTL to expire",
				"wopiSrc", rs.wopiSrc)
		}
	}

	m.mu.Lock()
	m.deleteRoomLocked(rs)
	m.mu.Unlock()
}

// deleteRoomLocked drops rs from every m.rooms key it is aliased under
// (several WOPISrc spellings can share one room), and from m.byFileID,
// guarded by cur == rs so this can never delete a different room's
// fileID index entry out from under it. The caller must hold m.mu.
func (m *Manager) deleteRoomLocked(rs *roomState) {
	for src := range rs.wopiSrcs {
		delete(m.rooms, src)
	}
	if rs.fileID != "" {
		if cur, ok := m.byFileID[rs.fileID]; ok && cur == rs {
			delete(m.byFileID, rs.fileID)
		}
	}
}

// checkTokenExpiry warns, once per token, when a tracked token drops
// under tokenExpiryWarnWindow of its own expiry.
func (m *Manager) checkTokenExpiry(rs *roomState, now time.Time) {
	// due copies expiresAt out under the lock, instead of carrying the
	// *tokenInfo pointer past m.mu.Unlock(): every field of
	// roomState/tokenInfo is guarded by m.mu, and reading info.expiresAt
	// after the unlock below would be an unguarded read racing a
	// concurrent Observe.
	type due struct {
		userID    string
		expiresAt time.Time
	}

	m.mu.Lock()
	var toWarn []due
	for userID, info := range rs.tokens {
		if info.warned || !now.Before(info.expiresAt) {
			continue
		}
		if info.expiresAt.Sub(now) <= tokenExpiryWarnWindow {
			info.warned = true
			toWarn = append(toWarn, due{userID, info.expiresAt})
		}
	}
	fileID := rs.fileID
	hook := m.onTokenExpiring
	m.mu.Unlock()

	for _, w := range toWarn {
		slog.Warn("room: access token nearing expiry", "fileID", fileID, "userID", w.userID, "expiresAt", w.expiresAt)
		if hook != nil {
			hook(fileID, w.userID)
		}
	}
}

// tokenAttemptResult classifies one ladder candidate's WOPI-call error, for
// classifyTokenAttempt below. rejected and noWrite are mutually exclusive
// with conflict != nil; none set means err was some other failure
// (network, host fault) for the caller's own generic-failure path.
type tokenAttemptResult struct {
	rejected bool
	noWrite  bool
	conflict *wopiclient.ErrLockConflict
}

// classifyTokenAttempt classifies one ladder candidate's failed WOPI call
// (Lock, RefreshLock, or PutFile). It centralizes error classification
// that performSave's two branches and refreshLock would otherwise each
// duplicate: ErrTokenRejected and ErrNoWriteAccess both get recorded
// against token right here, so the next tokenLadder build skips it; an
// ErrLockConflict is only reported back, not acted on, since its cascade
// differs enough across the three call sites (ensureLocked already
// handles its own conflict, PutFile's 409 has its own empty/foreign
// split, REFRESH_LOCK's empty case retries a full re-LOCK) to stay
// outside this helper.
func (m *Manager) classifyTokenAttempt(rs *roomState, now time.Time, token string, err error) tokenAttemptResult {
	if _, ok := errors.AsType[wopiclient.ErrTokenRejected](err); ok {
		m.markTokenFailed(rs, now, token)
		return tokenAttemptResult{rejected: true}
	}
	if _, ok := errors.AsType[wopiclient.ErrNoWriteAccess](err); ok {
		m.markTokenNoWrite(rs, now, token)
		return tokenAttemptResult{noWrite: true}
	}
	if conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err); ok {
		return tokenAttemptResult{conflict: &conflict}
	}
	return tokenAttemptResult{}
}

// markTokenFailed records that token was rejected outright (403), so the
// save ladder skips it for failedTokenTTL.
func (m *Manager) markTokenFailed(rs *roomState, now time.Time, token string) {
	m.mu.Lock()
	rs.failedTokens[token] = now
	m.mu.Unlock()
}

// markTokenNoWrite records that token got a 401 (ErrNoWriteAccess): the
// token is valid but the user's write ability is off. It both skips the
// token on the save ladder like markTokenFailed, and flips the tracked
// tokenInfo's canWrite so a durable permission change is not forgotten
// once failedTokenTTL elapses.
func (m *Manager) markTokenNoWrite(rs *roomState, now time.Time, token string) {
	m.mu.Lock()
	rs.failedTokens[token] = now
	for _, info := range rs.tokens {
		if info.token == token {
			info.canWrite = false
		}
	}
	m.mu.Unlock()
}

// recordSaveSuccess records a completed PutFile. seq is the rs.sceneSeq
// performSave captured when it snapshotted the scene it just sent: dirty
// clears only when no newer PutScene landed while that network call was
// in flight; otherwise the room stays dirty so the newer scene is not
// silently dropped as already saved.
func (m *Manager) recordSaveSuccess(rs *roomState, now time.Time, version string, seq uint64) {
	m.mu.Lock()
	if rs.sceneSeq == seq {
		rs.dirty = false
	}
	rs.lastSaveAttemptAt = now
	rs.expectedVersion = version
	rs.haveVersion = true
	rs.backoff = 0
	rs.nextRetryAt = time.Time{}
	rs.firstFailedSaveAt = time.Time{}
	rs.writeLost = false
	m.mu.Unlock()
}

// recordSaveFailure records one failed save pass. allNoWrite reports
// whether the pass tried at least one token and every candidate failure
// was ErrNoWriteAccess: only then does it set rs.writeLost, and it never
// clears the flag, so a later pass with an empty ladder cannot mask a
// durable write-access loss an earlier pass already found.
func (m *Manager) recordSaveFailure(rs *roomState, now time.Time, err error, allNoWrite bool) {
	m.mu.Lock()
	rs.lastSaveAttemptAt = now
	if rs.backoff == 0 {
		rs.backoff = initialBackoff
	} else {
		rs.backoff *= 2
		if rs.backoff > maxBackoff {
			rs.backoff = maxBackoff
		}
	}
	rs.nextRetryAt = now.Add(rs.backoff)
	if rs.firstFailedSaveAt.IsZero() {
		rs.firstFailedSaveAt = now
	}
	if allNoWrite {
		rs.writeLost = true
	}
	m.mu.Unlock()
	slog.Warn("room: save failed for every tracked token", "wopiSrc", rs.wopiSrc, "error", err)
}
