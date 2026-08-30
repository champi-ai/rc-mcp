package protocol

import "time"

// MessageType enumerates all JSON envelope message types.
type MessageType string

const (
	MsgHello        MessageType = "hello"
	MsgHelloAck     MessageType = "hello_ack"
	MsgPairRequest  MessageType = "pair_request"
	MsgPairCode     MessageType = "pair_code"
	MsgPairApproved MessageType = "pair_approved"
	MsgDispatch     MessageType = "dispatch"
	MsgResult       MessageType = "result"
	MsgProgress     MessageType = "progress"
	MsgError        MessageType = "error"
	MsgCancel       MessageType = "cancel"
	MsgPing         MessageType = "ping"
	MsgPong         MessageType = "pong"
	MsgClose        MessageType = "close"
)

// Envelope is the JSON envelope for all text WebSocket frames.
type Envelope struct {
	Type            MessageType `json:"type"`
	ID              string      `json:"id,omitempty"` // correlation ID (UUIDv4)
	ProtocolVersion string      `json:"protocolVersion,omitempty"`
	Ts              time.Time   `json:"ts"`
	Payload         any         `json:"payload,omitempty"` // type-specific; see per-type payloads below
}

// --- Per-type payloads ---

// HelloPayload is sent by the agent on initial connection.
type HelloPayload struct {
	DeviceToken  string   `json:"deviceToken"`
	Hostname     string   `json:"hostname"`
	Capabilities []string `json:"capabilities"` // e.g. ["shell","screenshot","filesystem","process","sysinfo"]
}

// HelloAckPayload is sent by the server to confirm authentication.
type HelloAckPayload struct {
	DeviceID string `json:"deviceId"`
	Resume   bool   `json:"resume"` // true if reconnecting within grace period
	// LatestAgentVersion is the version the operator has configured as
	// current for the fleet (AGENT_LATEST_VERSION on the server), for the
	// agent's auto-update check on connect (Section 19). Empty means the
	// server has no configured version to advertise -- an agent must
	// never attempt to update on an empty value. Introduced additively in
	// protocol version "2"; a "1"-only agent simply ignores the field.
	LatestAgentVersion string `json:"latestAgentVersion,omitempty"`
}

// PairRequestPayload is sent by an unpaired agent.
type PairRequestPayload struct {
	Hostname string `json:"hostname"`
}

// PairCodePayload is sent by the server with the pairing code.
type PairCodePayload struct {
	Code      string    `json:"code"`      // human-readable, e.g. "ABCD-1234"
	ExpiresAt time.Time `json:"expiresAt"` // code expiry (e.g. 5 minutes from now)
}

// PairApprovedPayload is sent by the server after operator approval.
type PairApprovedPayload struct {
	DeviceID    string `json:"deviceId"`
	DeviceToken string `json:"deviceToken"` // persistent bearer token for future connections
}

// DispatchPayload is sent by the server to dispatch a tool call to the agent.
type DispatchPayload struct {
	Tool      string `json:"tool"`
	RequestID string `json:"requestId"` // MCP JSON-RPC request ID for correlation
	SessionID string `json:"sessionId"` // MCP session ID (for server-side correlation, not agent use)
	Input     any    `json:"input"`     // tool-specific input, matches the tool's input schema
}

// ResultPayload is sent by the agent with the terminal result of a dispatch.
type ResultPayload struct {
	Tool    string `json:"tool"`
	Output  any    `json:"output"`          // tool-specific output, matches the tool's output schema
	IsError bool   `json:"isError"`         // true if the tool execution failed
	Error   string `json:"error,omitempty"` // error message if isError
}

// ProgressPayload is sent by the agent for streaming updates.
type ProgressPayload struct {
	Tool    string   `json:"tool"`
	Percent *float64 `json:"percent,omitempty"` // 0.0-100.0
	Message string   `json:"message,omitempty"`
}

// ErrorPayload is used for error messages.
type ErrorPayload struct {
	Code    string `json:"code"` // e.g. "version_mismatch", "auth_failed", "device_not_found"
	Message string `json:"message"`
}

// CancelPayload is sent by the server to cancel an in-flight dispatch.
type CancelPayload struct {
	Reason string `json:"reason,omitempty"` // e.g. "client_cancelled", "session_expired"
}

// ClosePayload is sent by the server to gracefully close a stream or connection.
type ClosePayload struct {
	Reason string `json:"reason,omitempty"` // e.g. "session_terminated", "server_shutdown"
}
