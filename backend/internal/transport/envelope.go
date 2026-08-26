package transport

import "encoding/json"

// Envelope is the typed JSON wrapper for WebSocket frames.
// Every WS message is an Envelope, so a client can dispatch on Type
// without inspecting the payload.
//
// Spec from project.prompt B3: {type, seq, payload}
//   - Type identifies the kind of message.
//   - Seq is the room's monotonic sequence number after the message's
//     effect (or the current seq for errors). Clients use it to order
//     broadcasts and as ExpectedSeq for optimistic concurrency.
//   - Payload is type-specific.
type Envelope struct {
	Type    string          `json:"type"`
	Seq     uint64          `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// mustMarshal is a helper for building payloads in handlers;
// it panics only on programmer error (marshalling a struct that is
// always serialisable), so the server never silently drops a broadcast.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("transport: marshal payload: " + err.Error())
	}
	return b
}
