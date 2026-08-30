package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/audit"
	"github.com/champi-ai/rc-mcp/internal/mcp/types"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/shellpolicy"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

// DefaultMaxShellSessions is the default RC_MAX_SHELL_SESSIONS cap (Section
// 3.1.2: "Max concurrent shell sessions per MCP session exceeded (default:
// 5)").
const DefaultMaxShellSessions = 5

const shellSessionStartInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "shell":    { "type": "string", "description": "Shell binary (default: $SHELL or /bin/bash)" },
    "cwd":      { "type": "string", "description": "Initial working directory (default: $HOME)" },
    "env":      { "type": "object", "additionalProperties": { "type": "string" }, "description": "Extra environment variables" },
    "rows":     { "type": "integer", "description": "PTY rows (default: 24)", "minimum": 1 },
    "cols":     { "type": "integer", "description": "PTY columns (default: 80)", "minimum": 1 }
  },
  "required": ["clientId"]
}`

const shellSessionStartOutputSchema = `{
  "type": "object",
  "properties": {
    "shellSessionId": { "type": "string" }, "pid": { "type": "integer" },
    "shell": { "type": "string" }, "clientId": { "type": "string" }
  },
  "required": ["shellSessionId", "pid", "shell", "clientId"]
}`

const shellSessionWriteInputSchema = `{
  "type": "object",
  "properties": {
    "shellSessionId": { "type": "string" },
    "input":          { "type": "string", "description": "Text to write to the PTY (include \\n for Enter)" }
  },
  "required": ["shellSessionId", "input"]
}`

const shellSessionWriteOutputSchema = `{
  "type": "object",
  "properties": {
    "bytesWritten": { "type": "integer" }, "output": { "type": "string" },
    "exitCode": { "type": "integer" }, "exited": { "type": "boolean" }
  },
  "required": ["bytesWritten"]
}`

const shellSessionCloseInputSchema = `{
  "type": "object",
  "properties": {
    "shellSessionId": { "type": "string" },
    "signal":         { "type": "string", "enum": ["SIGTERM", "SIGKILL"], "description": "default: SIGTERM, then SIGKILL after 5s" }
  },
  "required": ["shellSessionId"]
}`

const shellSessionCloseOutputSchema = `{
  "type": "object",
  "properties": { "exitCode": { "type": "integer" }, "finalOutput": { "type": "string" } },
  "required": ["exitCode"]
}`

const shellSessionStartAnnotations = `{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true}`
const shellSessionWriteAnnotations = `{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true}`
const shellSessionCloseAnnotations = `{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}`

// ShellSessionDeps are the dependencies shell_session_start/write/close
// handlers need.
type ShellSessionDeps struct {
	Bridge      ShellDispatcher
	Audit       *audit.Logger
	SkipConfirm bool
	// ConfirmEveryWrite requires per-write elicitation when true
	// (RC_SHELL_CONFIRM_EVERY_WRITE).
	ConfirmEveryWrite bool
	// MaxSessions <= 0 uses DefaultMaxShellSessions.
	MaxSessions int
	// ElicitationTimeout <= 0 uses transport.DefaultElicitationTimeout.
	ElicitationTimeout time.Duration
	// NotifySessionsChanged, if set, is invoked after a shell session is
	// opened or closed in sess, so the shell://sessions resource can push
	// an updated notification (Section 4.5).
	NotifySessionsChanged func(sess *session.Session)
	// Policy, if non-nil, is checked against shell_session_start's shell
	// binary (when specified) and every shell_session_write's input
	// before dispatch (RC_SHELL_ALLOWLIST/RC_SHELL_DENYLIST, Section 19).
	// A nil Policy allows everything.
	Policy *shellpolicy.Policy
}

func (d ShellSessionDeps) notifyChanged(sess *session.Session) {
	if d.NotifySessionsChanged != nil {
		d.NotifySessionsChanged(sess)
	}
}

func (d ShellSessionDeps) maxSessions() int {
	if d.MaxSessions <= 0 {
		return DefaultMaxShellSessions
	}
	return d.MaxSessions
}

func NewShellSessionStartDefinition(deps ShellSessionDeps) Definition {
	return Definition{
		Name: "shell_session_start", Title: "Start Interactive Shell Session",
		Description:        "Open a PTY-backed interactive shell on the target device, tied to this MCP session.",
		InputSchema:        json.RawMessage(shellSessionStartInputSchema),
		OutputSchema:       json.RawMessage(shellSessionStartOutputSchema),
		Annotations:        json.RawMessage(shellSessionStartAnnotations),
		RequiredCapability: "shell",
		Handler:            deps.handleStart,
	}
}

func NewShellSessionWriteDefinition(deps ShellSessionDeps) Definition {
	return Definition{
		Name: "shell_session_write", Title: "Write to Interactive Shell",
		Description:  "Send input (keystrokes/commands) to an open interactive shell session on the target device.",
		InputSchema:  json.RawMessage(shellSessionWriteInputSchema),
		OutputSchema: json.RawMessage(shellSessionWriteOutputSchema),
		Annotations:  json.RawMessage(shellSessionWriteAnnotations),
		Handler:      deps.handleWrite,
		// No RequiredCapability: the target device is resolved from the
		// shellSessionId mapping, not the "clientId" input field (there is
		// none), so the registry's generic capability check can't apply
		// here -- handleWrite enforces the mapping itself.
	}
}

func NewShellSessionCloseDefinition(deps ShellSessionDeps) Definition {
	return Definition{
		Name: "shell_session_close", Title: "Close Interactive Shell Session",
		Description:  "Terminate an interactive shell session on the target device and release its PTY.",
		InputSchema:  json.RawMessage(shellSessionCloseInputSchema),
		OutputSchema: json.RawMessage(shellSessionCloseOutputSchema),
		Annotations:  json.RawMessage(shellSessionCloseAnnotations),
		Handler:      deps.handleClose,
	}
}

func (d ShellSessionDeps) handleStart(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.ShellSessionStartInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}

	if len(sess.ListShellSessions()) >= d.maxSessions() {
		return toolError(fmt.Sprintf("Maximum concurrent shell sessions (%d) exceeded for this MCP session", d.maxSessions())), nil
	}

	start := time.Now()

	if input.Shell != nil {
		if allowed, reason := d.Policy.Check(*input.Shell); !allowed {
			d.logAudit(sess.ID, input.ClientID, "shell_session_start", input, audit.StatusBlocked, time.Since(start), reason)
			return toolError("Shell " + reason), nil
		}
	}

	if !d.SkipConfirm {
		message := fmt.Sprintf("Start an interactive shell session on device %s?", input.ClientID)
		schema, _ := json.Marshal(map[string]any{
			"type":       "object",
			"properties": map[string]any{"confirm": map[string]any{"type": "boolean", "description": message}},
			"required":   []string{"confirm"},
		})
		result := transport.RequestElicitation(ctx, sess, message, schema, d.ElicitationTimeout)
		if result.Declined {
			d.logAudit(sess.ID, input.ClientID, "shell_session_start", input, audit.StatusCancelled, time.Since(start), "declined")
			return declinedResult(result.Reason), nil
		}
		var confirmPayload struct {
			Confirm bool `json:"confirm"`
		}
		_ = json.Unmarshal(result.Content, &confirmPayload)
		if !confirmPayload.Confirm {
			d.logAudit(sess.ID, input.ClientID, "shell_session_start", input, audit.StatusCancelled, time.Since(start), "declined")
			return declinedResult("declined"), nil
		}
	}

	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}
	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, input.ClientID, correlationID, "shell_session_start", sess.ID, input, nil)
	duration := time.Since(start)

	if dispatchErr != nil {
		msg := dispatchErrorMessage(dispatchErr, input.ClientID)
		d.logAudit(sess.ID, input.ClientID, "shell_session_start", input, audit.StatusError, duration, dispatchErr.Error())
		return toolError(msg), nil
	}
	if resultPayload.IsError {
		d.logAudit(sess.ID, input.ClientID, "shell_session_start", input, audit.StatusError, duration, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}

	output, err := decodePayload[types.ShellSessionStartOutput](resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, input.ClientID, "shell_session_start", input, audit.StatusError, duration, err.Error())
		return toolError("failed to decode agent result"), nil
	}
	output.ClientID = input.ClientID

	sess.SetShellSession(output.ShellSessionID, &session.ShellSessionEntry{
		ClientID: input.ClientID, PID: output.PID, Shell: output.Shell, CreatedAt: time.Now().UTC(),
	})
	d.notifyChanged(sess)

	d.logAudit(sess.ID, input.ClientID, "shell_session_start", input, audit.StatusOK, duration, "")
	return marshalToolResult(output)
}

func (d ShellSessionDeps) handleWrite(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.ShellSessionWriteInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ShellSessionID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"shellSessionId\" is required"}
	}

	entry, ok := sess.GetShellSession(input.ShellSessionID)
	if !ok {
		return toolError(fmt.Sprintf("Shell session %s not found or already closed", input.ShellSessionID)), nil
	}

	start := time.Now()

	if allowed, reason := d.Policy.Check(input.Input); !allowed {
		d.logAudit(sess.ID, entry.ClientID, "shell_session_write", input, audit.StatusBlocked, time.Since(start), reason)
		return toolError("Input " + reason), nil
	}

	if d.ConfirmEveryWrite {
		message := fmt.Sprintf("Send input to shell session %s on device %s?", input.ShellSessionID, entry.ClientID)
		schema, _ := json.Marshal(map[string]any{
			"type":       "object",
			"properties": map[string]any{"confirm": map[string]any{"type": "boolean", "description": message}},
			"required":   []string{"confirm"},
		})
		result := transport.RequestElicitation(ctx, sess, message, schema, d.ElicitationTimeout)
		if result.Declined {
			d.logAudit(sess.ID, entry.ClientID, "shell_session_write", input, audit.StatusCancelled, time.Since(start), "declined")
			return declinedResult(result.Reason), nil
		}
		var confirmPayload struct {
			Confirm bool `json:"confirm"`
		}
		_ = json.Unmarshal(result.Content, &confirmPayload)
		if !confirmPayload.Confirm {
			d.logAudit(sess.ID, entry.ClientID, "shell_session_write", input, audit.StatusCancelled, time.Since(start), "declined")
			return declinedResult("declined"), nil
		}
	}

	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}

	onProgress := agent.EmitProgressAndBinary(sess, meta.ProgressToken, agent.DefaultEmitBackpressure)
	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, entry.ClientID, correlationID, "shell_session_write", sess.ID, input, onProgress)
	duration := time.Since(start)

	if dispatchErr != nil {
		msg := dispatchErrorMessage(dispatchErr, entry.ClientID)
		d.logAudit(sess.ID, entry.ClientID, "shell_session_write", input, audit.StatusError, duration, dispatchErr.Error())
		return toolError(msg), nil
	}
	if resultPayload.IsError {
		d.logAudit(sess.ID, entry.ClientID, "shell_session_write", input, audit.StatusError, duration, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}

	output, err := decodePayload[types.ShellSessionWriteOutput](resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, entry.ClientID, "shell_session_write", input, audit.StatusError, duration, err.Error())
		return toolError("failed to decode agent result"), nil
	}

	if output.Exited {
		sess.DeleteShellSession(input.ShellSessionID)
		d.notifyChanged(sess)
	}

	d.logAudit(sess.ID, entry.ClientID, "shell_session_write", input, audit.StatusOK, duration, "")
	return marshalToolResult(output)
}

func (d ShellSessionDeps) handleClose(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.ShellSessionCloseInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ShellSessionID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"shellSessionId\" is required"}
	}

	entry, ok := sess.GetShellSession(input.ShellSessionID)
	if !ok {
		return toolError(fmt.Sprintf("Shell session %s not found", input.ShellSessionID)), nil
	}
	// The server-side mapping is removed regardless of what happens next --
	// even an offline agent shouldn't leave this MCP session thinking the
	// shell session is still usable (Section 3.1.4: "Agent offline: tool
	// error; server cleans up its own mapping").
	defer func() {
		sess.DeleteShellSession(input.ShellSessionID)
		d.notifyChanged(sess)
	}()

	start := time.Now()
	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}
	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, entry.ClientID, correlationID, "shell_session_close", sess.ID, input, nil)
	duration := time.Since(start)

	if dispatchErr != nil {
		msg := dispatchErrorMessage(dispatchErr, entry.ClientID)
		d.logAudit(sess.ID, entry.ClientID, "shell_session_close", input, audit.StatusError, duration, dispatchErr.Error())
		return toolError(msg), nil
	}
	if resultPayload.IsError {
		d.logAudit(sess.ID, entry.ClientID, "shell_session_close", input, audit.StatusError, duration, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}

	output, err := decodePayload[types.ShellSessionCloseOutput](resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, entry.ClientID, "shell_session_close", input, audit.StatusError, duration, err.Error())
		return toolError("failed to decode agent result"), nil
	}

	d.logAudit(sess.ID, entry.ClientID, "shell_session_close", input, audit.StatusOK, duration, "")
	return marshalToolResult(output)
}

func (d ShellSessionDeps) logAudit(sessionID, clientID, tool string, input any, status string, duration time.Duration, errMsg string) {
	if d.Audit == nil {
		return
	}
	_ = d.Audit.LogCall(sessionID, clientID, tool, input, status, duration, errMsg)
}
