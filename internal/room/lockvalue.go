package room

import (
	"crypto/sha256"
	"encoding/hex"
)

// lockValuePrefix names the lock string as this service's, so an operator
// reading the host's lock records or another WOPI client's logs can tell
// which editor holds a file.
const lockValuePrefix = "excalidraw-wopi-"

// lockValueFor returns the deterministic WOPI lock value for fileID: the
// same file always yields the same value, so a restarted service, or a
// second replica racing on the same file, recognizes its own lock on
// sight instead of minting a fresh, unrecognizable one every time. It is
// keyed on the file id, not the WOPISrc string: two differently spelled
// WOPISrc values for the very same file would otherwise hash to two
// different lock values and each treat the other's lock as a foreign
// one.
func lockValueFor(fileID string) string {
	sum := sha256.Sum256([]byte(fileID))
	return lockValuePrefix + hex.EncodeToString(sum[:])[:16]
}
