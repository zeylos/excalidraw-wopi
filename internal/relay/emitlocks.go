package relay

import "sync"

// roomEmitLocks serializes one room's registry-mutate-then-broadcast
// sequence (onJoinRoom, onDisconnecting). The registry's own mutex orders
// the mutation itself, but each handler releases it before it emits
// room-user-change; two concurrent joins or leaves for the same room could
// otherwise compute their presence snapshots in one order under the
// registry mutex and emit them in the other order, so members would see a
// stale roster arrive after a newer one. Holding one lock per room across
// the whole mutate-then-emit sequence fixes that ordering without
// serializing unrelated rooms against each other.
type roomEmitLocks struct {
	mu    sync.Mutex
	locks map[string]*refcountedMutex
}

// refcountedMutex pairs a mutex with a count of callers that currently
// hold, or are waiting to acquire, this room's entry. Unlock must delete
// the map entry only once that count reaches zero: deleting it while
// another lock() caller already holds a reference to the same
// *sync.Mutex value, but has not locked it yet, would let a concurrent
// lock() call for the same room id create a second, distinct entry —
// two different mutexes then "protecting" one room, defeating the
// serialization roomEmitLocks exists for.
type refcountedMutex struct {
	mu   sync.Mutex
	refs int
}

func newRoomEmitLocks() *roomEmitLocks {
	return &roomEmitLocks{locks: make(map[string]*refcountedMutex)}
}

// lock returns roomID's emit lock, already held: the caller must release
// it (typically with defer) via unlock, exactly once, once its
// mutate-then-emit sequence is done.
func (r *roomEmitLocks) lock(roomID string) *heldEmitLock {
	r.mu.Lock()
	l, ok := r.locks[roomID]
	if !ok {
		l = &refcountedMutex{}
		r.locks[roomID] = l
	}
	l.refs++
	r.mu.Unlock()

	l.mu.Lock()
	return &heldEmitLock{locks: r, roomID: roomID, entry: l}
}

// heldEmitLock is the token lock() returns: it is a one-shot handle
// bound to the specific *refcountedMutex entry acquired, so Unlock
// always releases and dereferences the entry it actually locked even if
// the map no longer holds that entry for the room id.
type heldEmitLock struct {
	locks  *roomEmitLocks
	roomID string
	entry  *refcountedMutex
}

// Unlock releases the emit lock and drops this holder's reference; once
// no caller holds or awaits this room's entry, it is removed from the
// map so roomEmitLocks does not grow without bound over a server's
// lifetime.
func (h *heldEmitLock) Unlock() {
	h.locks.mu.Lock()
	h.entry.refs--
	if h.entry.refs == 0 {
		if cur, ok := h.locks.locks[h.roomID]; ok && cur == h.entry {
			delete(h.locks.locks, h.roomID)
		}
	}
	h.locks.mu.Unlock()

	h.entry.mu.Unlock()
}
