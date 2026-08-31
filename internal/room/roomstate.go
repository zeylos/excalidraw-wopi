package room

import (
	"sort"
	"time"
)

// tokenInfo is one tracked user's latest observed access token for a
// room.
type tokenInfo struct {
	token     string
	expiresAt time.Time
	canWrite  bool
	// warned marks that the token-expiry-TTL watch already fired its
	// warning for this token, so a room that stays
	// live past the warning does not log or call the hook again for it.
	warned bool
}

// tokenCandidate is one entry in a token ladder: a userID paired with
// the token tracked for it.
type tokenCandidate struct {
	userID string
	token  string
}

// roomState is one WOPISrc's live bookkeeping: the retained scene, the
// token registry, the lock and version markers, and the conflict and
// close-grace flags. Every field is guarded by the owning Manager's mu;
// nothing here has its own lock.
type roomState struct {
	// wopiSrc is the spelling every actual WOPI call (Lock, PutFile, ...)
	// uses: whichever spelling first created this room. A later,
	// differently spelled WOPISrc for the very same fileID aliases onto
	// this same roomState rather than minting a competing room, and is
	// recorded in wopiSrcs, not swapped in here.
	wopiSrc string
	// wopiSrcs holds every WOPISrc key m.rooms currently maps to this
	// room: normally just {wopiSrc}, but every alias spelling adds its
	// own key here too, so a close or GC pass (save.go's gcRoom,
	// performClose) can remove every one of them instead of leaving the
	// aliases as dangling map entries pointing at a room already gone.
	wopiSrcs map[string]struct{}
	fileID   string

	scene            []byte
	dirty            bool
	lastPutSceneAt   time.Time
	lastWriterUserID string
	// sceneSeq counts every PutScene call for this room. performSave
	// captures it when it snapshots rs.scene for the network call: if a
	// newer PutScene lands while that PutFile is still in flight, the
	// snapshot is stale, and recordSaveSuccess must leave the room dirty
	// instead of discarding the newer edit as already saved (the
	// lost-update bug this field exists to close).
	sceneSeq uint64

	tokens map[string]*tokenInfo // userID -> token info
	// failedTokens remembers, per token, when it last got ErrTokenRejected
	// or ErrNoWriteAccess, so the save loop's token ladder skips it for
	// failedTokenTTL: long enough to stop hammering a token that just
	// failed, short enough that a one-off host hiccup does not blacklist
	// it forever.
	failedTokens map[string]time.Time

	// liveUserCount is the relay's join count for this room
	// (OnJoin/OnLeave), independent of the boardapi token registry
	// above: a viewer can hold an open socket between REST
	// polls, so this is the more precise "is anyone here" signal for the
	// lock refresh loop and the close-grace timer.
	liveUserCount int

	lockValue  string
	haveLock   bool
	lastLockAt time.Time
	// forceUnlockValue is a one-shot value ResolveConflict(overwrite) sets
	// when a foreign lock was on record: the next ensureLocked call uses
	// UnlockAndRelock against it instead of a plain LOCK, since a foreign
	// lock otherwise never yields to an overwrite.
	forceUnlockValue string
	// skipVersionCheckOnce likewise one-shots past the next scheduled
	// CheckFileInfo comparison after a forced overwrite: expectedVersion
	// is stale by definition at that point.
	skipVersionCheckOnce bool

	expectedVersion string
	haveVersion     bool
	// pendingVersionCheck is set when a newly observed user joins an
	// established room; the background loop clears it once it has run
	// the CheckFileInfo comparison.
	pendingVersionCheck bool

	conflict            bool
	conflictForeignLock string
	// lastReportedConflict and lastReportedSaveStalled track what
	// notifyState last actually pushed to onConflictChange, so a repeat
	// pass with unchanged state fires no duplicate event.
	lastReportedConflict    bool
	lastReportedSaveStalled bool

	// lastSaveAttemptAt gates the ServerSaveInterval throttle; it is set
	// on every attempt, successful or not, so a failing save does not
	// retry faster than the schedule (outside of the backoff path below).
	lastSaveAttemptAt time.Time
	// backoff and nextRetryAt hold the retry schedule after every
	// tracked token failed a save: 1s, doubling, capped at 5 min, and
	// re-armed on every further failure.
	backoff     time.Duration
	nextRetryAt time.Time
	// firstSaveDone is set once, on the room's very first performSave
	// attempt ever, and never cleared: it is the sole gate on the
	// unlocked-PutFile fallback (save.go). lastSaveAttemptAt cannot serve
	// as that gate: ResolveConflict(overwrite) resets it to bypass the
	// throttle, which would otherwise re-arm the fallback on a room that
	// has already saved for real.
	firstSaveDone bool
	// firstFailedSaveAt tracks an ongoing failing streak: recordSaveFailure
	// sets it, recordSaveSuccess clears it. notifyState derives
	// saveStalled from how long the streak has run.
	firstFailedSaveAt time.Time
	// writeLost marks that every tracked token receives ErrNoWriteAccess
	// on the most recent save or lock-refresh pass; only a save success
	// sets it to false.
	writeLost bool

	closing bool
	closeAt time.Time
	// idleSince marks when this room first had nothing live, dirty, or
	// closing about it; the background loop GCs it after gcIdleTimeout,
	// for a room that never gets a clean OnLeave (a crashed tab, or one
	// only ever touched over REST, never through the relay).
	idleSince time.Time
	// lastStuckLogAt throttles the once-per-hour visibility log for a
	// conflicted room that stays dirty with no live users: its scene is
	// kept, never discarded, but an operator should still hear about it
	// periodically.
	lastStuckLogAt time.Time

	// inFlight guards against two goroutines from the bounded evaluation
	// pool running this room's evaluation, or Shutdown's flush, at the
	// same time.
	inFlight bool
}

// tokenLadder returns the token candidates a write operation should try,
// most preferred first: the most recent writer's token, then every
// other tracked, unexpired, write-capable, non-failed token,
// in a stable order (sorted by user id) so tests and logs are
// deterministic. The caller must hold the owning Manager's mu.
func (rs *roomState) tokenLadder(now time.Time) []tokenCandidate {
	var head *tokenCandidate
	var rest []tokenCandidate
	for userID, info := range rs.tokens {
		if !info.canWrite || !now.Before(info.expiresAt) || rs.tokenRecentlyFailedLocked(info.token, now) {
			continue
		}
		cand := tokenCandidate{userID: userID, token: info.token}
		if userID == rs.lastWriterUserID {
			c := cand
			head = &c
			continue
		}
		rest = append(rest, cand)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].userID < rest[j].userID })
	if head != nil {
		return append([]tokenCandidate{*head}, rest...)
	}
	return rest
}

// tokenRecentlyFailedLocked reports whether token failed within
// failedTokenTTL of now. The caller must hold the owning Manager's mu.
func (rs *roomState) tokenRecentlyFailedLocked(token string, now time.Time) bool {
	failedAt, ok := rs.failedTokens[token]
	if !ok {
		return false
	}
	return now.Sub(failedAt) < failedTokenTTL
}

// anyLiveToken returns a token that is merely unexpired, write ability
// aside, for a call that only needs read access (CheckFileInfo). It
// prefers a candidate from tokenLadder, since that token is already
// known to be write-capable and non-failed, and falls back to any other
// unexpired token. The caller must hold the owning Manager's mu.
func (rs *roomState) anyLiveToken(now time.Time) string {
	if ladder := rs.tokenLadder(now); len(ladder) > 0 {
		return ladder[0].token
	}
	for _, info := range rs.tokens {
		if now.Before(info.expiresAt) {
			return info.token
		}
	}
	return ""
}
