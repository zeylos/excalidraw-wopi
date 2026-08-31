package relay

import (
	"reflect"
	"testing"
)

func TestElectSyncerJoinOrder(t *testing.T) {
	members := []Member{
		{SocketID: "s1", UserID: "u1", CanWrite: true},
		{SocketID: "s2", UserID: "u2", CanWrite: true},
	}

	next, changed, promoted, demoted := electSyncer(members, "")
	if next != "u1" || !changed {
		t.Fatalf("electSyncer() = (%q, %v), want (\"u1\", true)", next, changed)
	}
	if want := []string{"s1"}; !reflect.DeepEqual(promoted, want) {
		t.Fatalf("promoted = %v, want %v", promoted, want)
	}
	if demoted != nil {
		t.Fatalf("demoted = %v, want nil (no previous syncer)", demoted)
	}
}

func TestElectSyncerReadOnlyExcluded(t *testing.T) {
	members := []Member{
		{SocketID: "s1", UserID: "u1", CanWrite: false},
		{SocketID: "s2", UserID: "u2", CanWrite: true},
	}

	next, changed, promoted, _ := electSyncer(members, "")
	if next != "u2" || !changed {
		t.Fatalf("electSyncer() = (%q, %v), want (\"u2\", true): a read-only member must never win the slot", next, changed)
	}
	if want := []string{"s2"}; !reflect.DeepEqual(promoted, want) {
		t.Fatalf("promoted = %v, want %v", promoted, want)
	}
}

func TestElectSyncerNoEligibleMemberClearsSlot(t *testing.T) {
	members := []Member{
		{SocketID: "s1", UserID: "u1", CanWrite: false},
	}

	next, changed, promoted, demoted := electSyncer(members, "")
	if next != "" || changed {
		t.Fatalf("electSyncer() = (%q, %v), want (\"\", false): no CanWrite member exists", next, changed)
	}
	if promoted != nil || demoted != nil {
		t.Fatalf("promoted/demoted = %v/%v, want nil/nil", promoted, demoted)
	}
}

func TestElectSyncerKeepsCurrentWhileItHoldsASocket(t *testing.T) {
	// The current syncer has two sockets (two tabs); one leaves. The
	// last-socket rule says this must not re-elect, even though u2 joined
	// earlier in the raw member order.
	members := []Member{
		{SocketID: "s2", UserID: "u2", CanWrite: true},
		{SocketID: "s1b", UserID: "u1", CanWrite: true}, // u1's remaining tab
	}

	next, changed, promoted, demoted := electSyncer(members, "u1")
	if next != "u1" || changed {
		t.Fatalf("electSyncer() = (%q, %v), want (\"u1\", false): u1 still holds a socket", next, changed)
	}
	if promoted != nil || demoted != nil {
		t.Fatalf("promoted/demoted = %v/%v, want nil/nil", promoted, demoted)
	}
}

func TestElectSyncerLastSocketElectsNext(t *testing.T) {
	// u1 was syncer; u1's last socket already left, so members no longer
	// lists u1. u2 joined next and can write, so u2 succeeds.
	members := []Member{
		{SocketID: "s2", UserID: "u2", CanWrite: true},
		{SocketID: "s3", UserID: "u3", CanWrite: true},
	}

	next, changed, promoted, demoted := electSyncer(members, "u1")
	if next != "u2" || !changed {
		t.Fatalf("electSyncer() = (%q, %v), want (\"u2\", true)", next, changed)
	}
	if want := []string{"s2"}; !reflect.DeepEqual(promoted, want) {
		t.Fatalf("promoted = %v, want %v", promoted, want)
	}
	// u1 holds no socket in members: this leave-driven case never demotes
	// anyone (u1 is already gone). See TestElectSyncerDemotesAStalePointer
	// for the general demotion bookkeeping.
	if demoted != nil {
		t.Fatalf("demoted = %v, want nil", demoted)
	}
}

func TestElectSyncerLastSocketWithNoSuccessorClearsSlot(t *testing.T) {
	members := []Member{
		{SocketID: "s2", UserID: "u2", CanWrite: false},
	}

	next, changed, promoted, demoted := electSyncer(members, "u1")
	if next != "" || !changed {
		t.Fatalf("electSyncer() = (%q, %v), want (\"\", true): no CanWrite member remains", next, changed)
	}
	if promoted != nil {
		t.Fatalf("promoted = %v, want nil", promoted)
	}
	if demoted != nil {
		t.Fatalf("demoted = %v, want nil: u1 already holds no socket", demoted)
	}
}

// TestDemotionTargetsListsEveryOutgoingSyncerSocket tests the demotion
// bookkeeping directly: a user must never keep believing it is the syncer
// once someone else takes over. electSyncer's own gate means its callers,
// registry.join and registry.leave, never actually observe a still-present
// old syncer losing the role (see electSyncer's doc comment), so this test
// exercises demotionTargets on its own terms, as the independently correct
// piece of plumbing it is.
func TestDemotionTargetsListsEveryOutgoingSyncerSocket(t *testing.T) {
	members := []Member{
		{SocketID: "s1", UserID: "u1", CanWrite: true},
		{SocketID: "s2a", UserID: "u2", CanWrite: true},
		{SocketID: "s2b", UserID: "u2", CanWrite: true},
	}

	got := demotionTargets(members, "u2", "u1")
	if want := []string{"s2a", "s2b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("demotionTargets() = %v, want %v: every socket of the outgoing syncer", got, want)
	}
}

func TestDemotionTargetsEmptyWhenSyncerUnchanged(t *testing.T) {
	members := []Member{{SocketID: "s1", UserID: "u1", CanWrite: true}}
	if got := demotionTargets(members, "u1", "u1"); got != nil {
		t.Fatalf("demotionTargets() = %v, want nil: no reassignment happened", got)
	}
}

func TestDemotionTargetsEmptyWhenNoPreviousSyncer(t *testing.T) {
	members := []Member{{SocketID: "s1", UserID: "u1", CanWrite: true}}
	if got := demotionTargets(members, "", "u1"); got != nil {
		t.Fatalf("demotionTargets() = %v, want nil: there was nobody to demote", got)
	}
}

// TestElectSyncerCurrentWithOnlyReadOnlySocketReelects covers the case where
// u1 was syncer; its write socket already left, but a read-only socket for
// the same user id remains. hasSocket must not count that as u1 "still
// holding a socket", or the room would stay wedged on a syncer that can no
// longer save. This is also the one path through registry.join/leave that
// reaches demotionTargets with a non-empty result (see electSyncer's doc
// comment): u1's read-only socket is still present in members, so the
// reelection must demote it explicitly.
func TestElectSyncerCurrentWithOnlyReadOnlySocketReelects(t *testing.T) {
	members := []Member{
		{SocketID: "s1ro", UserID: "u1", CanWrite: false},
		{SocketID: "s2", UserID: "u2", CanWrite: true},
	}

	next, changed, promoted, demoted := electSyncer(members, "u1")
	if next != "u2" || !changed {
		t.Fatalf("electSyncer() = (%q, %v), want (\"u2\", true): u1's only remaining socket is read-only", next, changed)
	}
	if want := []string{"s2"}; !reflect.DeepEqual(promoted, want) {
		t.Fatalf("promoted = %v, want %v", promoted, want)
	}
	if want := []string{"s1ro"}; !reflect.DeepEqual(demoted, want) {
		t.Fatalf("demoted = %v, want %v: u1's read-only socket must be told isSyncer:false", demoted, want)
	}
}

func TestRegistryJoinElectsFirstWriter(t *testing.T) {
	r := newRegistry()

	_, _, isSyncer := r.join("room1", Member{SocketID: "s1", UserID: "u1", CanWrite: false})
	if isSyncer {
		t.Fatal("a read-only joiner must never become syncer")
	}

	_, _, isSyncer = r.join("room1", Member{SocketID: "s2", UserID: "u2", CanWrite: true})
	if !isSyncer {
		t.Fatal("the first CanWrite joiner must claim the empty syncer slot")
	}

	_, _, isSyncer = r.join("room1", Member{SocketID: "s3", UserID: "u3", CanWrite: true})
	if isSyncer {
		t.Fatal("a later joiner must not preempt an existing syncer")
	}

	// A second tab for the existing syncer reports true too.
	_, _, isSyncer = r.join("room1", Member{SocketID: "s2b", UserID: "u2", CanWrite: true})
	if !isSyncer {
		t.Fatal("a second socket for the syncer's own user must also report isSyncer true")
	}
}

func TestRegistryLeavePromotesOnlyOnLastSocket(t *testing.T) {
	r := newRegistry()

	r.join("room1", Member{SocketID: "s1a", UserID: "u1", CanWrite: true})
	r.join("room1", Member{SocketID: "s1b", UserID: "u1", CanWrite: true})
	r.join("room1", Member{SocketID: "s2", UserID: "u2", CanWrite: true})

	_, _, outcome := r.leave("room1", "s1a")
	if outcome.Changed {
		t.Fatalf("outcome = %+v, want unchanged: u1 still holds s1b", outcome)
	}

	_, _, outcome = r.leave("room1", "s1b")
	if !outcome.Changed {
		t.Fatalf("outcome = %+v, want a change promoting u2", outcome)
	}
	if want := []string{"s2"}; !reflect.DeepEqual(outcome.PromotedIDs, want) {
		t.Fatalf("promoted = %v, want %v", outcome.PromotedIDs, want)
	}
}
