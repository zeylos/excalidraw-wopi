package relay

// electionOutcome is the result of a syncer recompute (registry.join or
// registry.leave). The caller turns a Changed outcome into sync-designate
// emits: true to PromotedIDs, false to DemotedIDs.
type electionOutcome struct {
	Changed     bool
	PromotedIDs []string
	DemotedIDs  []string
}

// electSyncer recomputes a room's syncer from its current members (join
// order) and the previously stored syncer userID ("" for none).
//
// current keeps the role while it still holds a socket in members: a join
// or a non-last-socket departure never reassigns it. Once current holds
// no socket, the earliest joined CanWrite member takes over, or the room
// goes syncer-less when no CanWrite member remains.
//
// PromotedIDs and DemotedIDs name the sockets a caller must push a
// sync-designate emit to (see demotionTargets). DemotedIDs is non-empty
// whenever current is still present in members but hasSocket no longer
// counts it: the one case this reaches is a syncer whose last CanWrite
// socket left while a read-only socket for the same user id remains (see
// TestElectSyncerCurrentWithOnlyReadOnlySocketReelects in syncer_test.go).
func electSyncer(members []Member, current string) (next string, changed bool, promoted, demoted []string) {
	if current != "" && hasSocket(members, current) {
		return current, false, nil, nil
	}

	next = electNext(members)
	if next == current {
		return current, false, nil, nil
	}
	if next != "" {
		promoted = socketIDsForUser(members, next)
	}
	return next, true, promoted, demotionTargets(members, current, next)
}

// demotionTargets returns the sockets that must be told isSyncer:false
// when the syncer changes from oldSyncer to newSyncer: every socket
// oldSyncer still holds in members. A user must never keep believing it
// is the syncer once someone else takes over. oldSyncer is usually
// already absent from members by the time this runs (its last socket
// left), so the result is usually empty; the reachable non-empty case is
// a syncer whose only remaining socket turned read-only, which still
// holds a member row here even though hasSocket no longer counts it.
func demotionTargets(members []Member, oldSyncer, newSyncer string) []string {
	if oldSyncer == "" || oldSyncer == newSyncer {
		return nil
	}
	return socketIDsForUser(members, oldSyncer)
}

// electNext returns the earliest-joined CanWrite user in members, or ""
// when none qualifies. A user's rank follows their earliest socket still
// present in members, the same first-occurrence order presence() uses.
func electNext(members []Member) string {
	seen := make(map[string]bool, len(members))
	for _, m := range members {
		if seen[m.UserID] {
			continue
		}
		seen[m.UserID] = true
		if m.CanWrite {
			return m.UserID
		}
	}
	return ""
}

// hasSocket reports whether userID still holds a CanWrite socket in
// members. A read-only socket does not count: if the current syncer's
// only remaining connection is read-only, the slot must still fall to the
// next eligible writer, or a room whose write session already left would
// stay wedged on a syncer that can no longer save.
func hasSocket(members []Member, userID string) bool {
	for _, m := range members {
		if m.UserID == userID && m.CanWrite {
			return true
		}
	}
	return false
}

// socketIDsForUser returns every socket id userID holds in members, in
// join order.
func socketIDsForUser(members []Member, userID string) []string {
	if userID == "" {
		return nil
	}
	var out []string
	for _, m := range members {
		if m.UserID == userID {
			out = append(out, m.SocketID)
		}
	}
	return out
}
