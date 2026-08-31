package relay

// members returns a copy of roomID's members in join order. Test-only:
// production code never needs a room's raw member list on its own,
// only the presence view join/leave already return. Kept under the
// registry's own lock so a test can call it while another goroutine
// mutates the registry concurrently (see
// TestOnDisconnectingRunsAfterAPendingJoinRoomTask).
func (r *registry) members(roomID string) []Member {
	r.mu.Lock()
	defer r.mu.Unlock()

	members := r.rooms[roomID]
	out := make([]Member, len(members))
	copy(out, members)
	return out
}
