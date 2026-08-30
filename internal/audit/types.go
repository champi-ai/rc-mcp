// Package audit implements the server-side, append-only audit log: the
// authoritative, tamper-resistant record of every tool invocation. See
// docs/specs/backend.md Section 12.7.
package audit

import (
	"encoding/json"
	"time"
)

// Entry is a single append-only audit log record, serialized as one line
// of newline-delimited JSON.
type Entry struct {
	Timestamp  time.Time `json:"timestamp"`
	SessionID  string    `json:"sessionId"` // MCP session
	ClientID   string    `json:"clientId"`  // target device
	Tool       string    `json:"tool"`
	ArgsDigest string    `json:"argsDigest"` // SHA-256 of the JSON-encoded args
	ArgsHint   string    `json:"argsHint"`   // sanitized summary, e.g. "command=ls -la"
	// FullArgs carries the complete, unredacted tool arguments when the
	// operator has opted into RC_AUDIT_FULL_ARGS=true (Section 12.7).
	// Absent (the default) when full-args logging is disabled -- ArgsDigest
	// and ArgsHint remain the only record of arguments in that case.
	FullArgs   json.RawMessage `json:"fullArgs,omitempty"`
	Status     string          `json:"status"` // ok, error, cancelled, blocked
	DurationMs int64           `json:"durationMs"`
	Error      string          `json:"error,omitempty"`
}

// Status values for Entry.Status.
const (
	StatusOK        = "ok"
	StatusError     = "error"
	StatusCancelled = "cancelled"
	// StatusBlocked marks a call the server refused to dispatch at all
	// (e.g. a shell allowlist/denylist match, Section 19) -- distinct from
	// StatusError, which implies the agent attempted and failed it.
	StatusBlocked = "blocked"
)
