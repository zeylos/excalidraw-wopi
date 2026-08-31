package peers

import (
	"crypto/sha256"
	"encoding/binary"
)

// rendezvousOwner picks the peer with the highest rendezvous-hash score
// for fileID. This is a pure function of (fileID, peersList): every
// replica that sees the same peer set picks the same owner, with no
// coordination between replicas.
func rendezvousOwner(fileID string, peersList []string) string {
	var best string
	var bestScore uint64
	for _, p := range peersList {
		score := rendezvousScore(fileID, p)
		if best == "" || score > bestScore || (score == bestScore && p > best) {
			best = p
			bestScore = score
		}
	}
	return best
}

func rendezvousScore(fileID, peer string) uint64 {
	h := sha256.Sum256([]byte(fileID + "\x00" + peer))
	return binary.BigEndian.Uint64(h[:8])
}
