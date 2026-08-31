package relay

import (
	"encoding/json"
	"testing"
)

func TestRewriteVolatileMouseLocationOverwritesUser(t *testing.T) {
	raw := []byte(`{"type":"MOUSE_LOCATION","payload":{"pointer":{"x":1,"y":2},"user":{"id":"attacker","name":"Evil"}}}`)
	identity := Session{UserID: "u1", UserName: "Alice"}

	out, ok := rewriteVolatile(raw, identity)
	if !ok {
		t.Fatal("rewriteVolatile() ok = false, want true")
	}

	var got struct {
		Type    string `json:"type"`
		Payload struct {
			Pointer struct {
				X, Y float64
			} `json:"pointer"`
			User presenceUser `json:"user"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode rewritten payload: %v", err)
	}
	if got.Type != "MOUSE_LOCATION" {
		t.Fatalf("type = %q, want MOUSE_LOCATION", got.Type)
	}
	if got.Payload.User.ID != "u1" || got.Payload.User.Name != "Alice" {
		t.Fatalf("user = %+v, want the server identity, not the forged one", got.Payload.User)
	}
	if got.Payload.Pointer.X != 1 || got.Payload.Pointer.Y != 2 {
		t.Fatalf("pointer = %+v, want the client's original fields preserved", got.Payload.Pointer)
	}
}

func TestRewriteVolatileViewportUpdateOverwritesUserID(t *testing.T) {
	raw := []byte(`{"type":"VIEWPORT_UPDATE","payload":{"userId":"attacker","scrollX":10}}`)
	identity := Session{UserID: "u1", UserName: "Alice"}

	out, ok := rewriteVolatile(raw, identity)
	if !ok {
		t.Fatal("rewriteVolatile() ok = false, want true")
	}

	var got struct {
		Type    string `json:"type"`
		Payload struct {
			UserID  string  `json:"userId"`
			ScrollX float64 `json:"scrollX"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode rewritten payload: %v", err)
	}
	if got.Payload.UserID != "u1" {
		t.Fatalf("userId = %q, want u1 (server identity), not the forged one", got.Payload.UserID)
	}
	if got.Payload.ScrollX != 10 {
		t.Fatalf("scrollX = %v, want the client's original field preserved", got.Payload.ScrollX)
	}
}

func TestRewriteVolatileDropsUnknownType(t *testing.T) {
	raw := []byte(`{"type":"IMAGE_ADD","payload":{}}`)
	if _, ok := rewriteVolatile(raw, Session{UserID: "u1"}); ok {
		t.Fatal("rewriteVolatile() ok = true, want false: only MOUSE_LOCATION and VIEWPORT_UPDATE relay")
	}
}

func TestRewriteVolatileDropsMalformedJSON(t *testing.T) {
	if _, ok := rewriteVolatile([]byte(`not json`), Session{UserID: "u1"}); ok {
		t.Fatal("rewriteVolatile() ok = true, want false for malformed JSON")
	}
}

func TestRewriteVolatileDropsMalformedPayload(t *testing.T) {
	raw := []byte(`{"type":"MOUSE_LOCATION","payload":"not an object"}`)
	if _, ok := rewriteVolatile(raw, Session{UserID: "u1"}); ok {
		t.Fatal("rewriteVolatile() ok = true, want false: payload is not a JSON object")
	}
}

func TestExceedsSceneLimit(t *testing.T) {
	tests := []struct {
		name     string
		size     int
		max      int64
		exceeded bool
	}{
		{"under the cap", 10, 20, false},
		{"exactly at the cap", 20, 20, false},
		{"over the cap", 21, 20, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exceedsSceneLimit(make([]byte, tt.size), tt.max); got != tt.exceeded {
				t.Fatalf("exceedsSceneLimit(%d bytes, max %d) = %v, want %v", tt.size, tt.max, got, tt.exceeded)
			}
		})
	}
}

func TestIsIdentityBearingTypeFlagsMouseLocation(t *testing.T) {
	payload := []byte(`{"type":"MOUSE_LOCATION","payload":{"user":{"id":"attacker","name":"Evil"}}}`)
	if !isIdentityBearingType(payload) {
		t.Fatal("isIdentityBearingType() = false, want true: MOUSE_LOCATION must never reach server-broadcast unchecked")
	}
}

func TestIsIdentityBearingTypeFlagsViewportUpdate(t *testing.T) {
	payload := []byte(`{"type":"VIEWPORT_UPDATE","payload":{"userId":"attacker"}}`)
	if !isIdentityBearingType(payload) {
		t.Fatal("isIdentityBearingType() = false, want true: VIEWPORT_UPDATE must never reach server-broadcast unchecked")
	}
}

func TestIsIdentityBearingTypeAllowsSceneTypes(t *testing.T) {
	for _, typ := range []string{"SCENE_INIT", "SCENE_UPDATE", "IMAGE_ADD"} {
		payload := []byte(`{"type":"` + typ + `","payload":{}}`)
		if isIdentityBearingType(payload) {
			t.Fatalf("isIdentityBearingType(%s) = true, want false: scene types must keep relaying unchecked", typ)
		}
	}
}

func TestIsIdentityBearingTypeIgnoresMalformedJSON(t *testing.T) {
	if isIdentityBearingType([]byte("not json")) {
		t.Fatal("isIdentityBearingType() = true, want false: malformed bytes are not the two guarded types, so the caller must keep forwarding them untouched")
	}
}

func TestImageRequestBytesShape(t *testing.T) {
	raw, err := imageRequestBytes("file-123")
	if err != nil {
		t.Fatalf("imageRequestBytes() error = %v", err)
	}

	var got struct {
		Type    string `json:"type"`
		Payload struct {
			FileID string `json:"fileId"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode image request payload: %v", err)
	}
	if got.Type != "IMAGE_REQUEST" {
		t.Fatalf("type = %q, want IMAGE_REQUEST", got.Type)
	}
	if got.Payload.FileID != "file-123" {
		t.Fatalf("payload.fileId = %q, want file-123", got.Payload.FileID)
	}
}
