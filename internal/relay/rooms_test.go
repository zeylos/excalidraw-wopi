package relay

import (
	"reflect"
	"testing"
)

func TestRegistryJoinDedupesByUser(t *testing.T) {
	r := newRegistry()

	list, added, _ := r.join("room1", Member{SocketID: "s1", UserID: "u1", UserName: "Alice", CanWrite: true})
	if !added {
		t.Fatal("first join for s1 must report added")
	}
	if want := []UserPresence{{SocketID: "s1", UserID: "u1", UserName: "Alice", SocketIDs: []string{"s1"}}}; !reflect.DeepEqual(list, want) {
		t.Fatalf("presence = %+v, want %+v", list, want)
	}

	// A second tab for the same user gets its own socket, but the two
	// collapse into one presence row.
	list, added, _ = r.join("room1", Member{SocketID: "s2", UserID: "u1", UserName: "Alice", CanWrite: true})
	if !added {
		t.Fatal("join for a new socket id must report added")
	}
	want := []UserPresence{{SocketID: "s1", UserID: "u1", UserName: "Alice", SocketIDs: []string{"s1", "s2"}}}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("presence = %+v, want %+v", list, want)
	}
}

func TestRegistryJoinIsIdempotentPerSocket(t *testing.T) {
	r := newRegistry()

	r.join("room1", Member{SocketID: "s1", UserID: "u1", UserName: "Alice"})
	list, added, _ := r.join("room1", Member{SocketID: "s1", UserID: "u1", UserName: "Alice"})
	if added {
		t.Fatal("a repeat join for the same socket id must not report added")
	}
	if got := len(list); got != 1 {
		t.Fatalf("presence rows = %d, want 1", got)
	}
	if got := len(list[0].SocketIDs); got != 1 {
		t.Fatalf("socket ids for the user = %d, want 1 (no duplicate)", got)
	}
}

func TestRegistryPreservesJoinOrder(t *testing.T) {
	r := newRegistry()

	r.join("room1", Member{SocketID: "s1", UserID: "u1", UserName: "Alice"})
	r.join("room1", Member{SocketID: "s2", UserID: "u2", UserName: "Bob"})
	r.join("room1", Member{SocketID: "s3", UserID: "u3", UserName: "Carol"})

	list, _, _ := r.join("room1", Member{SocketID: "s1", UserID: "u1", UserName: "Alice"})
	gotOrder := make([]string, len(list))
	for i, u := range list {
		gotOrder[i] = u.UserID
	}
	if want := []string{"u1", "u2", "u3"}; !reflect.DeepEqual(gotOrder, want) {
		t.Fatalf("user order = %v, want %v", gotOrder, want)
	}

	members := r.members("room1")
	gotSocketOrder := make([]string, len(members))
	for i, m := range members {
		gotSocketOrder[i] = m.SocketID
	}
	if want := []string{"s1", "s2", "s3"}; !reflect.DeepEqual(gotSocketOrder, want) {
		t.Fatalf("member (join) order = %v, want %v", gotSocketOrder, want)
	}
}

func TestRegistryLeaveRemovesOnlyThatSocket(t *testing.T) {
	r := newRegistry()

	r.join("room1", Member{SocketID: "s1", UserID: "u1", UserName: "Alice"})
	r.join("room1", Member{SocketID: "s2", UserID: "u1", UserName: "Alice"})
	r.join("room1", Member{SocketID: "s3", UserID: "u2", UserName: "Bob"})

	list, remaining, _ := r.leave("room1", "s2")
	if !remaining {
		t.Fatal("room1 must still have members after s2 leaves")
	}

	want := []UserPresence{
		{SocketID: "s1", UserID: "u1", UserName: "Alice", SocketIDs: []string{"s1"}},
		{SocketID: "s3", UserID: "u2", UserName: "Bob", SocketIDs: []string{"s3"}},
	}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("presence = %+v, want %+v", list, want)
	}
}

func TestRegistryLeaveLastMemberDropsRoom(t *testing.T) {
	r := newRegistry()

	r.join("room1", Member{SocketID: "s1", UserID: "u1", UserName: "Alice"})
	list, remaining, _ := r.leave("room1", "s1")
	if remaining {
		t.Fatal("room1 must be empty after its last member leaves")
	}
	if list != nil {
		t.Fatalf("presence = %+v, want nil", list)
	}

	if _, ok := r.rooms["room1"]; ok {
		t.Fatal("an emptied room must be dropped from the registry")
	}
}

func TestRegistryLeaveUnknownSocketIsANoop(t *testing.T) {
	r := newRegistry()

	r.join("room1", Member{SocketID: "s1", UserID: "u1", UserName: "Alice"})
	list, remaining, _ := r.leave("room1", "does-not-exist")
	if !remaining {
		t.Fatal("room1 must still report members")
	}
	if got := len(list); got != 1 {
		t.Fatalf("presence rows = %d, want 1", got)
	}
}

// TestRegistryJoinReadOnlyJoinerSharingSyncerUserIsNotSyncer checks the
// join-side half: a read-only joiner whose user id matches the room's
// syncer must not be told isSyncer true, even though the plain userID
// match would otherwise say so.
func TestRegistryJoinReadOnlyJoinerSharingSyncerUserIsNotSyncer(t *testing.T) {
	r := newRegistry()

	_, _, isSyncer := r.join("room1", Member{SocketID: "s1", UserID: "u1", CanWrite: true})
	if !isSyncer {
		t.Fatal("the first CanWrite joiner must claim the empty syncer slot")
	}

	// u1's second tab, this one read-only.
	_, _, isSyncer = r.join("room1", Member{SocketID: "s1b", UserID: "u1", CanWrite: false})
	if isSyncer {
		t.Fatal("a read-only joiner must not be told isSyncer true, even sharing the syncer's user id")
	}
}

func TestRegistryRoomsAreIndependent(t *testing.T) {
	r := newRegistry()

	r.join("room1", Member{SocketID: "s1", UserID: "u1", UserName: "Alice"})
	r.join("room2", Member{SocketID: "s2", UserID: "u1", UserName: "Alice"})

	if got := len(r.members("room1")); got != 1 {
		t.Fatalf("room1 members = %d, want 1", got)
	}
	if got := len(r.members("room2")); got != 1 {
		t.Fatalf("room2 members = %d, want 1", got)
	}
}
