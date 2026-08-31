package peers

import (
	"fmt"
	"testing"
)

func TestRendezvousOwnerIsDeterministic(t *testing.T) {
	peersList := []string{"http://a", "http://b", "http://c"}
	want := rendezvousOwner("file-1", peersList)
	for i := range 100 {
		if got := rendezvousOwner("file-1", peersList); got != want {
			t.Fatalf("rendezvousOwner() = %q on call %d, want %q (not deterministic)", got, i, want)
		}
	}
}

func TestRendezvousOwnerAlwaysPicksAPeer(t *testing.T) {
	peersList := []string{"http://a", "http://b", "http://c"}
	valid := map[string]bool{"http://a": true, "http://b": true, "http://c": true}
	for i := range 200 {
		fileID := fmt.Sprintf("file-%d", i)
		owner := rendezvousOwner(fileID, peersList)
		if !valid[owner] {
			t.Fatalf("rendezvousOwner(%q) = %q, want one of %v", fileID, owner, peersList)
		}
	}
}

// TestRendezvousOwnerMinimalDisruption checks the core rendezvous-hashing
// property: removing one peer from the set remaps only the files that
// peer owned. A file whose owner survives the removal must keep the same
// owner, since minimal disruption on membership change is the entire
// reason to use rendezvous hashing instead of a plain hash-mod-N.
func TestRendezvousOwnerMinimalDisruption(t *testing.T) {
	full := []string{"http://a", "http://b", "http://c"}
	reduced := []string{"http://a", "http://b"}

	const n = 200
	for i := range n {
		fileID := fmt.Sprintf("file-%d", i)
		before := rendezvousOwner(fileID, full)
		if before == "http://c" {
			continue // this file's owner was removed; it is expected to move.
		}
		after := rendezvousOwner(fileID, reduced)
		if after != before {
			t.Errorf("file %q: owner moved from %q to %q after an unrelated peer was removed", fileID, before, after)
		}
	}
}
