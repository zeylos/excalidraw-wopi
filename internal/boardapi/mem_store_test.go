package boardapi_test

import (
	"sync"

	"github.com/zeylos/excalidraw-wopi/internal/boardapi"
)

// memStore is a trivial, concurrency-safe in-memory RoomStore. It keeps
// only the latest scene per WOPISrc, unbounded, and it holds nothing
// across a restart. It exists for this package's tests only; production
// wires internal/room's Manager as RoomStore instead.
type memStore struct {
	mu     sync.RWMutex
	scenes map[string]storedScene
}

type storedScene struct {
	data []byte
	meta boardapi.SceneMeta
}

// newMemStore builds an empty memStore.
func newMemStore() *memStore {
	return &memStore{scenes: make(map[string]storedScene)}
}

// PutScene stores a copy of data, so a caller reusing its buffer cannot
// mutate what memStore holds.
func (m *memStore) PutScene(wopiSrc string, data []byte, meta boardapi.SceneMeta) error {
	cp := make([]byte, len(data))
	copy(cp, data)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.scenes[wopiSrc] = storedScene{data: cp, meta: meta}
	return nil
}

// GetScene returns a copy of the stored scene, so a caller mutating the
// result cannot corrupt what memStore holds.
func (m *memStore) GetScene(wopiSrc string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.scenes[wopiSrc]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(s.data))
	copy(cp, s.data)
	return cp, true
}
