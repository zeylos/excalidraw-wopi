package hostadapter

import (
	"errors"
	"net/http"
	"testing"

	"github.com/zeylos/excalidraw-wopi/internal/wopiclient"
)

func TestMapErrorTokenRejected(t *testing.T) {
	err := NewDrive().MapError("CheckFileInfo", http.StatusForbidden, "")
	if _, ok := errors.AsType[wopiclient.ErrTokenRejected](err); !ok {
		t.Errorf("MapError(403) = %v, want ErrTokenRejected", err)
	}
}

func TestMapErrorNoWriteAccess(t *testing.T) {
	err := NewDrive().MapError("Lock", http.StatusUnauthorized, "")
	if _, ok := errors.AsType[wopiclient.ErrNoWriteAccess](err); !ok {
		t.Errorf("MapError(401) = %v, want ErrNoWriteAccess", err)
	}
}

func TestMapErrorLockConflictCarriesCurrentLock(t *testing.T) {
	err := NewDrive().MapError("Lock", http.StatusConflict, "the-other-lock")
	conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err)
	if !ok {
		t.Fatalf("MapError(409) = %v, want ErrLockConflict", err)
	}
	if conflict.CurrentLock != "the-other-lock" {
		t.Errorf("CurrentLock = %q, want the-other-lock", conflict.CurrentLock)
	}
}

func TestMapErrorLockConflictWithExpiredLock(t *testing.T) {
	err := NewDrive().MapError("PutFile", http.StatusConflict, "")
	conflict, ok := errors.AsType[wopiclient.ErrLockConflict](err)
	if !ok {
		t.Fatalf("MapError(409) = %v, want ErrLockConflict", err)
	}
	if conflict.CurrentLock != "" {
		t.Errorf("CurrentLock = %q, want empty", conflict.CurrentLock)
	}
}

func TestMapErrorNotFound(t *testing.T) {
	err := NewDrive().MapError("PutFile", http.StatusNotFound, "")
	if _, ok := errors.AsType[wopiclient.ErrNotFound](err); !ok {
		t.Errorf("MapError(404) = %v, want ErrNotFound", err)
	}
}

func TestMapErrorTooLarge(t *testing.T) {
	for _, status := range []int{http.StatusPreconditionFailed, http.StatusRequestEntityTooLarge} {
		err := NewDrive().MapError("GetFile", status, "")
		if _, ok := errors.AsType[wopiclient.ErrTooLarge](err); !ok {
			t.Errorf("MapError(%d) = %v, want ErrTooLarge", status, err)
		}
	}
}

func TestMapErrorProofRejected(t *testing.T) {
	err := NewDrive().MapError("CheckFileInfo", http.StatusInternalServerError, "")
	if _, ok := errors.AsType[wopiclient.ErrProofRejected](err); !ok {
		t.Errorf("MapError(500) = %v, want ErrProofRejected", err)
	}
}

func TestMapErrorUnmappedStatusReturnsNil(t *testing.T) {
	if err := NewDrive().MapError("CheckFileInfo", http.StatusTeapot, ""); err != nil {
		t.Errorf("MapError(418) = %v, want nil", err)
	}
}

func TestItemVersionReadsHeader(t *testing.T) {
	h := http.Header{}
	h.Set("X-WOPI-ItemVersion", "etag-abc")
	if got := NewDrive().ItemVersion(h); got != "etag-abc" {
		t.Errorf("ItemVersion() = %q, want etag-abc", got)
	}
}

func TestItemVersionMissingHeaderIsEmpty(t *testing.T) {
	if got := NewDrive().ItemVersion(http.Header{}); got != "" {
		t.Errorf("ItemVersion() = %q, want empty", got)
	}
}
