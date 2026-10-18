// Package protocol implements the versioned newline-delimited JSON protocol
// shared by the Go terminal UI and the persistent Python worker.
package protocol

import "encoding/json"

const Version = 1

type Request struct {
	Version int            `json:"version"`
	ID      string         `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

// Message deliberately keeps Data and Error raw: forecast payloads are large
// and evolve independently of the UI, and events must retain unknown fields.
type Message struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Event   string          `json:"event,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func NewRequest(id, method string, params map[string]any) Request {
	if params == nil {
		params = map[string]any{}
	}
	return Request{Version: Version, ID: id, Method: method, Params: params}
}
