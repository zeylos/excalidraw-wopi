package room

import (
	"context"

	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

// Client is the subset of *wopiclient.Client the Manager calls. A plain
// *wopiclient.Client satisfies it; tests substitute a fake to force
// specific error sequences (a token rejection, a lock conflict) without a
// live or fake WOPI host.
type Client interface {
	CheckFileInfo(ctx context.Context, src, token string) (wopiclient.FileInfo, error)
	PutFile(ctx context.Context, src, token, lock string, body []byte) (string, error)
	Lock(ctx context.Context, src, token, lock string) error
	RefreshLock(ctx context.Context, src, token, lock string) error
	Unlock(ctx context.Context, src, token, lock string) error
	// UnlockAndRelock releases oldLock and acquires newLock in one call.
	// ensureLocked uses it to force an overwrite through against a
	// foreign lock still on record.
	UnlockAndRelock(ctx context.Context, src, token, newLock, oldLock string) error
}
