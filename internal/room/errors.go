package room

import "errors"

// errSceneTooLarge reports that PutScene's defense-in-depth size check
// (see Config.MaxSceneBytes) rejected a body boardapi should already have
// rejected before calling in.
var errSceneTooLarge = errors.New("room: scene exceeds the configured size limit")

// errShutdownFlushIncomplete reports that Shutdown's flush pass left at
// least one room still dirty when its context ran out or every tracked
// token failed.
var errShutdownFlushIncomplete = errors.New("room: shutdown flush left one or more rooms unsaved")

// errNoUsableToken reports that a save had no tracked, unexpired,
// write-capable, non-failed token to try.
var errNoUsableToken = errors.New("room: no usable token tracked for this room")
