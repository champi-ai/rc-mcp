package transport

import (
	"context"
	"encoding/json"

	"github.com/champi-ai/rc-mcp/internal/session"
)

// ToolDescriptor is the tools/list representation of a single registered
// tool (docs/specs/backend.md Section 3).
type ToolDescriptor struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
}

// ToolContent is one element of a tools/call result's "content" array.
type ToolContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// ToolCallResult is the result of a tools/call, per the MCP spec's tool
// result shape.
type ToolCallResult struct {
	Content           []ToolContent   `json:"content"`
	IsError           bool            `json:"isError,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
}

// ToolCallMeta carries the request-scoped metadata a tool handler needs
// beyond its input arguments.
type ToolCallMeta struct {
	// RequestID is the JSON-RPC request id of the originating tools/call,
	// used to correlate notifications/cancelled and register a CancelFunc
	// on the session.
	RequestID string
	// ProgressToken is the _meta.progressToken from the request, if any.
	// Long-running tools (shell_exec) use it to key
	// notifications/progress events they emit via Session.Emit.
	ProgressToken string
}

// RPCError is a protocol-level JSON-RPC error (as opposed to a tool
// execution error, which is reported via ToolCallResult.IsError). See
// docs/specs/backend.md Section 13.
type RPCError struct {
	Code    int
	Message string
	// Data is attached as the JSON-RPC error's "data" member when non-nil
	// (e.g. {"validationErrors": [...]} for -32602, Section 13).
	Data any
}

// ToolRegistry is implemented by internal/mcp/tools.Registry and wired
// into the transport Handler by cmd/server/main.go. It is declared here
// (rather than imported) so internal/transport has no dependency on
// internal/mcp, keeping the transport layer usable and testable
// independently of any concrete tool implementation.
type ToolRegistry interface {
	// ListTools returns the tools currently visible to tools/list: the
	// union of capabilities enabled across all online agents.
	ListTools(ctx context.Context) []ToolDescriptor
	// CallTool routes name to its registered handler and blocks until the
	// tool completes (dispatch pattern (b) -- see Section 9). The handler
	// may emit notifications/progress via sess.Emit while running.
	// A non-nil *RPCError indicates a protocol-level failure (e.g. unknown
	// tool name, invalid params); anything else is reported through the
	// returned ToolCallResult's IsError field.
	CallTool(ctx context.Context, sess *session.Session, meta ToolCallMeta, name string, args json.RawMessage) (*ToolCallResult, *RPCError)
}

// emptyRegistry is used when a Handler is constructed with no ToolRegistry,
// so tools/list returns an empty set and tools/call always reports
// "method not found" rather than the Handler panicking on a nil interface.
type emptyRegistry struct{}

func (emptyRegistry) ListTools(context.Context) []ToolDescriptor { return nil }

func (emptyRegistry) CallTool(context.Context, *session.Session, ToolCallMeta, string, json.RawMessage) (*ToolCallResult, *RPCError) {
	return nil, &RPCError{Code: codeMethodNotFound, Message: "no tools registered"}
}
