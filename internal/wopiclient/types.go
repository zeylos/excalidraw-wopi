package wopiclient

import "fmt"

// FileInfo holds the CheckFileInfo response fields the service uses.
// Field names follow the MS-WOPI property names; json tags map to the
// exact casing a WOPI host sends.
type FileInfo struct {
	BaseFileName string `json:"BaseFileName"`
	OwnerID      string `json:"OwnerId"`
	// UserFriendlyName is empty for an anonymous user.
	UserFriendlyName string `json:"UserFriendlyName"`
	Size             int64  `json:"Size"`
	UserID           string `json:"UserId"`
	// Version is the host's conflict-detection marker for the file.
	Version      string `json:"Version"`
	UserCanWrite bool   `json:"UserCanWrite"`
	ReadOnly     bool   `json:"ReadOnly"`

	SupportsUpdate  bool `json:"SupportsUpdate"`
	SupportsGetLock bool `json:"SupportsGetLock"`
	SupportsLocks   bool `json:"SupportsLocks"`
}

// ErrLockConflict reports a 409 response to a lock or save operation.
// CurrentLock is the value from the response's X-WOPI-Lock header. It is
// empty when the host's lock had already expired.
type ErrLockConflict struct {
	CurrentLock string
}

func (e ErrLockConflict) Error() string {
	return fmt.Sprintf("wopiclient: lock conflict, current lock %q", e.CurrentLock)
}

// ErrTokenRejected reports that the host rejected the access token as
// invalid, expired, or revoked.
type ErrTokenRejected struct{}

func (ErrTokenRejected) Error() string { return "wopiclient: access token rejected" }

// ErrNoWriteAccess reports that the access token is valid but does not
// carry write ability for the requested operation.
type ErrNoWriteAccess struct{}

func (ErrNoWriteAccess) Error() string { return "wopiclient: token has no write access" }

// ErrProofRejected reports a 500 from the WOPI host on a signed request.
// The host profile maps a 500 status to this error. See
// internal/hostadapter for why Drive answers 500 on a bad proof
// signature. A genuine host fault also gives a 500, and the status
// alone cannot tell the two apart.
type ErrProofRejected struct{}

func (ErrProofRejected) Error() string {
	return "wopiclient: the WOPI host returned 500: either the proof signature is wrong or the host itself failed; check the host's logs"
}

// ErrTooLarge reports that a request or response body exceeded a size
// limit the host enforces.
type ErrTooLarge struct{}

func (ErrTooLarge) Error() string { return "wopiclient: body exceeds the host size limit" }

// ErrNotFound reports that the host found no resource at the request URL.
type ErrNotFound struct{}

func (ErrNotFound) Error() string { return "wopiclient: not found" }

// HTTPError reports an HTTP failure that carries no host-specific meaning.
type HTTPError struct {
	Op     string
	Status int
	Body   string
}

func (e HTTPError) Error() string {
	return fmt.Sprintf("wopiclient: %s: unexpected status %d: %s", e.Op, e.Status, e.Body)
}
