package room

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

// errFakeLockFailure is a generic, non-token, non-conflict Lock failure a
// test can script: it stands in for a transport error or an
// unrecognized host fault, distinct from the two errors Client's callers
// specifically branch on.
var errFakeLockFailure = errors.New("fakeClient: simulated lock failure")

// fakeClient is a Client whose per-token behavior a test scripts up
// front, so the save-throttle and token-ladder tests can force an exact
// error sequence (a 403 on one token, success on another) without a real
// or fake WOPI host. It records every call for assertions.
type fakeClient struct {
	mu sync.Mutex

	// rejectTokens names tokens every call rejects with ErrTokenRejected.
	rejectTokens map[string]bool
	// noWriteTokens names tokens every call rejects with
	// ErrNoWriteAccess: a token that is valid but carries no write
	// ability.
	noWriteTokens map[string]bool
	// noWritePutTokens names tokens whose Lock call succeeds but whose
	// PutFile call rejects with ErrNoWriteAccess: write ability drops
	// between LOCK and PutFile, so a test can pin the writeLost tracking
	// through performSave's PutFile branch, not just its ensureLocked
	// branch.
	noWritePutTokens map[string]bool
	// genericFailTokens names tokens whose PutFile call fails with
	// errFakeLockFailure: a non-token, non-write-access failure kind, so
	// a test can force performSave's writeLost tracking to see a mixed
	// pass (one token 401s, another hits a different failure).
	genericFailTokens map[string]bool
	// failLockGeneric makes every Lock call fail with errFakeLockFailure,
	// regardless of token, so a test can force performSave's
	// unlocked-PutFile fallback without a token rejection or a lock
	// conflict.
	failLockGeneric bool
	// lockConflictQueue lets a test script an exact sequence of
	// ErrLockConflict responses for one token's successive Lock calls: the
	// real wopitest.Host can never 409 a LOCK with an empty CurrentLock (an
	// empty current lock always just succeeds there), so this is the only
	// way to simulate ensureLocked's empty-lock retry itself landing on a
	// foreign lock (bug: the retry path must enter conflict too).
	lockConflictQueue map[string][]wopiclient.ErrLockConflict
	// nextVersion is the X-WOPI-ItemVersion PutFile returns; it
	// increments on every successful PutFile, mimicking Drive's ETag.
	nextVersion int

	// putDelay, when set, has PutFile sleep before it records the call,
	// widening the window a race test needs to observe two PutFile calls
	// overlapping if the per-room inFlight guard ever failed to
	// serialize them.
	putDelay time.Duration
	// inFlightPuts and maxInFlightPuts track how many PutFile calls are
	// concurrently in progress, for that same race test; both are
	// updated with atomic ops so -race and concurrent callers agree.
	inFlightPuts    int32
	maxInFlightPuts int32

	lockCalls    []call
	refreshCalls []call
	unlockCalls  []call
	relockCalls  []relockCall
	putCalls     []putCall
	checkCalls   []call
}

type call struct {
	token string
	lock  string
}

type relockCall struct {
	token            string
	newLock, oldLock string
}

type putCall struct {
	token string
	lock  string
	body  []byte
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		rejectTokens:      map[string]bool{},
		noWriteTokens:     map[string]bool{},
		noWritePutTokens:  map[string]bool{},
		genericFailTokens: map[string]bool{},
	}
}

func (c *fakeClient) reject(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejectTokens[token] = true
}

// unreject undoes a prior reject call, so a test can recover a token after
// simulating a transient rejection.
func (c *fakeClient) unreject(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.rejectTokens, token)
}

func (c *fakeClient) CheckFileInfo(_ context.Context, _, token string) (wopiclient.FileInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkCalls = append(c.checkCalls, call{token: token})
	if c.rejectTokens[token] {
		return wopiclient.FileInfo{}, wopiclient.ErrTokenRejected{}
	}
	return wopiclient.FileInfo{Version: c.versionLocked()}, nil
}

func (c *fakeClient) PutFile(_ context.Context, _, token, lock string, body []byte) (string, error) {
	n := atomic.AddInt32(&c.inFlightPuts, 1)
	for {
		cur := atomic.LoadInt32(&c.maxInFlightPuts)
		if n <= cur || atomic.CompareAndSwapInt32(&c.maxInFlightPuts, cur, n) {
			break
		}
	}
	if c.putDelay > 0 {
		time.Sleep(c.putDelay)
	}
	defer atomic.AddInt32(&c.inFlightPuts, -1)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.putCalls = append(c.putCalls, putCall{token: token, lock: lock, body: append([]byte(nil), body...)})
	if c.rejectTokens[token] {
		return "", wopiclient.ErrTokenRejected{}
	}
	if c.noWriteTokens[token] || c.noWritePutTokens[token] {
		return "", wopiclient.ErrNoWriteAccess{}
	}
	if c.genericFailTokens[token] {
		return "", errFakeLockFailure
	}
	c.nextVersion++
	return c.versionLocked(), nil
}

func (c *fakeClient) Lock(_ context.Context, _, token, lock string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lockCalls = append(c.lockCalls, call{token: token, lock: lock})
	if c.rejectTokens[token] {
		return wopiclient.ErrTokenRejected{}
	}
	if c.noWriteTokens[token] {
		return wopiclient.ErrNoWriteAccess{}
	}
	if c.failLockGeneric {
		return errFakeLockFailure
	}
	if queue := c.lockConflictQueue[token]; len(queue) > 0 {
		conflict := queue[0]
		c.lockConflictQueue[token] = queue[1:]
		return conflict
	}
	return nil
}

// queueLockConflicts scripts token's next len(conflicts) Lock calls to
// return each conflict in order, before falling back to success.
func (c *fakeClient) queueLockConflicts(token string, conflicts ...wopiclient.ErrLockConflict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lockConflictQueue == nil {
		c.lockConflictQueue = map[string][]wopiclient.ErrLockConflict{}
	}
	c.lockConflictQueue[token] = append(c.lockConflictQueue[token], conflicts...)
}

func (c *fakeClient) RefreshLock(_ context.Context, _, token, lock string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshCalls = append(c.refreshCalls, call{token: token, lock: lock})
	if c.rejectTokens[token] {
		return wopiclient.ErrTokenRejected{}
	}
	if c.noWriteTokens[token] {
		return wopiclient.ErrNoWriteAccess{}
	}
	return nil
}

func (c *fakeClient) Unlock(_ context.Context, _, token, lock string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unlockCalls = append(c.unlockCalls, call{token: token, lock: lock})
	if c.rejectTokens[token] {
		return wopiclient.ErrTokenRejected{}
	}
	return nil
}

func (c *fakeClient) UnlockAndRelock(_ context.Context, _, token, newLock, oldLock string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.relockCalls = append(c.relockCalls, relockCall{token: token, newLock: newLock, oldLock: oldLock})
	if c.rejectTokens[token] {
		return wopiclient.ErrTokenRejected{}
	}
	return nil
}

// versionLocked stringifies nextVersion; the caller must hold c.mu.
func (c *fakeClient) versionLocked() string {
	return strconv.Itoa(c.nextVersion)
}

func (c *fakeClient) putCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.putCalls)
}

func (c *fakeClient) lockCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lockCalls)
}

func (c *fakeClient) unlockCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.unlockCalls)
}

func (c *fakeClient) lastPut() putCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.putCalls[len(c.putCalls)-1]
}
