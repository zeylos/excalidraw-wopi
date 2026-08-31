package relay

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// TestRoomEmitLocksSerializePerRoom checks the ordering guarantee directly:
// a second lock() for the same room must block until the first holder's
// critical section finishes, so the caller's compute-then-emit sequences
// cannot interleave.
func TestRoomEmitLocksSerializePerRoom(t *testing.T) {
	locks := newRoomEmitLocks()

	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		l := locks.lock("room1")
		defer l.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		order = append(order, "a")
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(5 * time.Millisecond) // let "a" grab the lock first
		l := locks.lock("room1")
		defer l.Unlock()
		mu.Lock()
		order = append(order, "b")
		mu.Unlock()
	}()
	close(start)
	wg.Wait()

	if want := []string{"a", "b"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v: the second lock() must wait for the first's critical section", order, want)
	}
}

// TestRoomEmitLocksDoNotBlockAcrossRooms proves the lock is per-room, not
// global: a held lock for room1 must not stall a lock() for room2.
func TestRoomEmitLocksDoNotBlockAcrossRooms(t *testing.T) {
	locks := newRoomEmitLocks()

	l1 := locks.lock("room1")
	defer l1.Unlock()

	done := make(chan struct{})
	go func() {
		l2 := locks.lock("room2")
		l2.Unlock()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lock(\"room2\") blocked on room1's held lock")
	}
}

// TestRoomEmitLocksEntryIsRemovedOnceUnheld proves the map entry does not
// leak one per room forever: once every holder has unlocked, a later
// lock() for the same room id gets a fresh, unlocked entry rather than
// growing the map without bound over a server's lifetime.
func TestRoomEmitLocksEntryIsRemovedOnceUnheld(t *testing.T) {
	locks := newRoomEmitLocks()

	l := locks.lock("room1")
	l.Unlock()

	if _, stillPresent := locks.locks["room1"]; stillPresent {
		t.Fatal("locks[\"room1\"] still present after its only holder unlocked, want the entry removed")
	}

	done := make(chan struct{})
	go func() {
		l2 := locks.lock("room1")
		l2.Unlock()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lock(\"room1\") after the entry was removed did not complete")
	}
}

// TestRoomEmitLocksRefcountKeepsOneMutexForConcurrentWaiters checks that
// removing a room's map entry only from inside Unlock, once nobody holds
// or awaits it, keeps one mutex protecting one room id. Deleting the entry
// as soon as the room emptied, from inside the same critical section that
// still held its mutex, would let a lock() call racing in during that
// window find no entry and build a second, brand-new mutex — two
// different mutexes then "protecting" the same room id, letting two
// holders run concurrently. Refcounting closes that window: a lock()
// racing in while the first holder is still inside its critical section
// must always block on that same holder's mutex, never get a fresh one.
func TestRoomEmitLocksRefcountKeepsOneMutexForConcurrentWaiters(t *testing.T) {
	locks := newRoomEmitLocks()

	first := locks.lock("room1")

	waiterEntered := make(chan struct{})
	waiterDone := make(chan struct{})
	go func() {
		close(waiterEntered)
		l := locks.lock("room1") // must block until first.Unlock()
		l.Unlock()
		close(waiterDone)
	}()
	<-waiterEntered
	time.Sleep(20 * time.Millisecond) // give the waiter time to reach its blocking Lock()

	select {
	case <-waiterDone:
		t.Fatal("the concurrent lock() call returned before the first holder unlocked: it must have gotten a different, fresh mutex instead of waiting on the same one")
	default:
	}

	first.Unlock()

	select {
	case <-waiterDone:
	case <-time.After(time.Second):
		t.Fatal("the waiting lock() call never completed after the first holder unlocked")
	}
}
