// Package hostadapter holds every Drive-specific WOPI quirk, so the rest
// of the service can speak plain WOPI through internal/wopiclient. v1
// targets Drive only; a later host gets its own profile in this package.
package hostadapter

import (
	"net/http"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

const (
	// ClientSaveInterval is the throttle between the syncer's saves to the
	// Go server.
	ClientSaveInterval = 10 * time.Second
	// ServerSaveInterval is the throttle between the Go server's PutFile
	// calls to the WOPI host.
	ServerSaveInterval = 60 * time.Second
	// LockRefreshInterval is how often the service refreshes a held lock.
	LockRefreshInterval = 10 * time.Minute
	// LockTTL is the lifetime Drive assigns to a lock in its cache.
	LockTTL = 30 * time.Minute
)

const itemVersionHeader = "X-WOPI-ItemVersion"

// Drive is the WOPI host profile for La Suite Drive. It implements
// wopiclient.HostProfile.
type Drive struct{}

// NewDrive returns the Drive host profile.
func NewDrive() Drive { return Drive{} }

// MapError maps a Drive response status to a typed wopiclient error. It
// returns nil when the status carries no Drive-specific meaning; the
// caller then falls back to a generic error.
//
// Drive's quirks: an invalid, expired, or revoked access token gets 403,
// not the spec's 401 (Drive's DRF authentication layer downgrades it). A
// 401 means the token is valid but the user lacks write ability for the
// operation. A proof-signature failure is an uncaught exception in
// Drive, so it surfaces as 500, not a 4xx status; a genuine Drive fault
// also surfaces as 500, and the two are not distinguishable here.
func (Drive) MapError(_ string, status int, wopiLockHeader string) error {
	switch status {
	case http.StatusForbidden:
		return wopiclient.ErrTokenRejected{}
	case http.StatusUnauthorized:
		return wopiclient.ErrNoWriteAccess{}
	case http.StatusConflict:
		return wopiclient.ErrLockConflict{CurrentLock: wopiLockHeader}
	case http.StatusNotFound:
		return wopiclient.ErrNotFound{}
	case http.StatusPreconditionFailed, http.StatusRequestEntityTooLarge:
		return wopiclient.ErrTooLarge{}
	case http.StatusInternalServerError:
		return wopiclient.ErrProofRejected{}
	default:
		return nil
	}
}

// ItemVersion reads the version marker Drive returns on GetFile and
// PutFile responses. Drive sets it to the S3 ETag of the stored file.
func (Drive) ItemVersion(header http.Header) string {
	return header.Get(itemVersionHeader)
}

var _ wopiclient.HostProfile = (*Drive)(nil)
