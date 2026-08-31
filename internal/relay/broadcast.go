package relay

import "encoding/json"

// typeMouseLocation and typeViewportUpdate are the two payload types that
// carry a client-asserted identity field (payload.user, payload.userId).
// The relay rewrites that field to the session's own identity when these
// types travel the volatile channel (rewriteVolatile); onServerBroadcast
// uses the same names to refuse them on the durable channel, where no
// rewrite ever runs: a writer could otherwise spoof another user's cursor
// by sending one of these on server-broadcast instead.
const (
	typeMouseLocation  = "MOUSE_LOCATION"
	typeViewportUpdate = "VIEWPORT_UPDATE"
)

// volatileEnvelope is the wire shape server-volatile-broadcast carries.
// Unlike server-broadcast, which forwards a payload untouched, this
// channel is parsed so the relay can overwrite the client-asserted
// identity with the session's own.
type volatileEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// rewriteVolatile parses raw as a server-volatile-broadcast envelope and
// overwrites the identity field the sender must never control:
// payload.user for MOUSE_LOCATION, payload.userId for VIEWPORT_UPDATE. It
// returns the re-encoded envelope to relay as client-broadcast, and false
// for malformed JSON or any other type, both of which the caller drops.
func rewriteVolatile(raw []byte, identity Session) ([]byte, bool) {
	var env volatileEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false
	}

	var payload map[string]any
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return nil, false
		}
	}
	if payload == nil {
		payload = map[string]any{}
	}

	switch env.Type {
	case typeMouseLocation:
		payload["user"] = presenceUser{ID: identity.UserID, Name: identity.UserName}
	case typeViewportUpdate:
		payload["userId"] = identity.UserID
	default:
		return nil, false
	}

	out, err := json.Marshal(volatileOut{Type: env.Type, Payload: payload})
	if err != nil {
		return nil, false
	}
	return out, true
}

type volatileOut struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// exceedsSceneLimit reports whether payload exceeds the configured scene
// byte cap.
func exceedsSceneLimit(payload []byte, maxSceneBytes int64) bool {
	return int64(len(payload)) > maxSceneBytes
}

// isIdentityBearingType peeks payload's JSON "type" tag and reports whether
// it is one of the two types whose identity field only the volatile
// channel rewrites (typeMouseLocation, typeViewportUpdate). It does not
// otherwise parse or alter payload: onServerBroadcast must forward every
// other server-broadcast type's bytes exactly as received.
func isIdentityBearingType(payload []byte) bool {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return false
	}
	return env.Type == typeMouseLocation || env.Type == typeViewportUpdate
}

// imageRequestPayload is the client-broadcast payload image-get relays to
// the room: a peer answers with the image, keyed by fileId.
type imageRequestPayload struct {
	Type    string              `json:"type"`
	Payload imageRequestFileRef `json:"payload"`
}

type imageRequestFileRef struct {
	FileID string `json:"fileId"`
}

// imageRequestBytes builds the UTF-8 JSON bytes for an IMAGE_REQUEST
// client-broadcast payload naming imageID.
func imageRequestBytes(imageID string) ([]byte, error) {
	return json.Marshal(imageRequestPayload{
		Type:    "IMAGE_REQUEST",
		Payload: imageRequestFileRef{FileID: imageID},
	})
}
