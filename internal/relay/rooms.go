package relay

import "sync"

// Member is one socket's presence in a room.
type Member struct {
	SocketID string
	UserID   string
	UserName string
	CanWrite bool
}

// UserPresence is one distinct user's row in a room-user-change payload. A
// user can hold several sockets at once (several tabs), so SocketIDs can
// list more than one entry while SocketID names a single representative
// socket.
type UserPresence struct {
	SocketID  string
	UserID    string
	UserName  string
	SocketIDs []string
}

// registry tracks room membership and the elected syncer. It is the
// single source of truth the relay's socket.io room join mirrors. A mutex
// guards it because handlers for different sockets run concurrently.
//
// Each room's member slice keeps join order. Syncer election (syncer.go)
// picks the earliest-joined writer, so this order is a load-bearing
// invariant, not an implementation detail.
type registry struct {
	mu      sync.Mutex
	rooms   map[string][]Member
	syncers map[string]string // roomID -> current syncer userID, "" or absent for none
}

func newRegistry() *registry {
	return &registry{rooms: make(map[string][]Member), syncers: make(map[string]string)}
}

// join adds m to roomID, unless a member with the same SocketID is already
// there, and runs syncer election. It returns the room's presence list
// after the change, whether m was newly added, and whether m's user holds
// the syncer role after the join.
func (r *registry) join(roomID string, m Member) (presenceList []UserPresence, added bool, isSyncer bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	members := r.rooms[roomID]
	for _, existing := range members {
		if existing.SocketID == m.SocketID {
			return presence(members), false, r.syncers[roomID] == m.UserID
		}
	}

	members = append(members, m)
	r.rooms[roomID] = members

	next, _, _, _ := electSyncer(members, r.syncers[roomID])
	r.syncers[roomID] = next
	// next == m.UserID alone is not enough: m's own socket must also be
	// CanWrite, or a read-only joiner sharing the syncer's user id (a
	// second, read-only tab for that same user) would be told isSyncer
	// true despite being unable to act as one.
	return presence(members), true, next == m.UserID && m.CanWrite
}

// leave removes the member with socketID from roomID and re-elects the
// syncer when the departure changes it. It returns the room's presence
// list after the change, whether the room still has members, and the
// election outcome for the caller to turn into sync-designate emits. When
// the room becomes empty, leave drops it from the registry.
func (r *registry) leave(roomID, socketID string) (presenceList []UserPresence, remaining bool, outcome electionOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()

	members := r.rooms[roomID]
	idx := -1
	for i, m := range members {
		if m.SocketID == socketID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return presence(members), len(members) > 0, electionOutcome{}
	}

	members = append(members[:idx:idx], members[idx+1:]...)
	if len(members) == 0 {
		delete(r.rooms, roomID)
		delete(r.syncers, roomID)
		return nil, false, electionOutcome{}
	}
	r.rooms[roomID] = members

	next, changed, promoted, demoted := electSyncer(members, r.syncers[roomID])
	if changed {
		r.syncers[roomID] = next
	}
	return presence(members), true, electionOutcome{
		Changed:     changed,
		PromotedIDs: promoted,
		DemotedIDs:  demoted,
	}
}

// presence collapses members into one row per distinct UserID, in
// first-join order, for the room-user-change payload.
func presence(members []Member) []UserPresence {
	if len(members) == 0 {
		return nil
	}

	order := make([]string, 0, len(members))
	byUser := make(map[string]*UserPresence, len(members))
	for _, m := range members {
		entry, ok := byUser[m.UserID]
		if !ok {
			entry = &UserPresence{
				SocketID: m.SocketID,
				UserID:   m.UserID,
				UserName: m.UserName,
			}
			byUser[m.UserID] = entry
			order = append(order, m.UserID)
		}
		entry.SocketIDs = append(entry.SocketIDs, m.SocketID)
	}

	out := make([]UserPresence, len(order))
	for i, userID := range order {
		out[i] = *byUser[userID]
	}
	return out
}
