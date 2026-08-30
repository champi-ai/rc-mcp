package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/audit"
	"github.com/champi-ai/rc-mcp/internal/mcp/types"
	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/shellpolicy"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

// ShellDispatcher is the minimal interface shell_exec needs from the
// dispatch bridge, satisfied by *agent.Bridge. Declared as an interface so
// tests can inject a fake dispatcher without a live agent WebSocket
// connection.
type ShellDispatcher interface {
	Dispatch(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error)
}

const shellExecInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "command":  { "type": "string", "description": "Command to execute (passed to /bin/sh -c)" },
    "cwd":      { "type": "string", "description": "Working directory (default: $HOME)" },
    "env":      { "type": "object", "additionalProperties": { "type": "string" }, "description": "Extra environment variables" },
    "timeout":  { "type": "integer", "description": "Max execution time in seconds (default: 30, max: 300)", "minimum": 1, "maximum": 300 },
    "stdin":    { "type": "string", "description": "Optional stdin to pipe into the command" }
  },
  "required": ["clientId", "command"]
}`

const shellExecOutputSchema = `{
  "type": "object",
  "properties": {
    "stdout":     { "type": "string" },
    "stderr":     { "type": "string" },
    "exitCode":   { "type": "integer" },
    "killed":     { "type": "boolean", "description": "True if the process was killed due to timeout" },
    "durationMs": { "type": "integer" },
    "clientId":   { "type": "string" }
  },
  "required": ["stdout", "stderr", "exitCode", "killed", "durationMs", "clientId"]
}`

const shellExecAnnotations = `{
  "readOnlyHint": false,
  "destructiveHint": true,
  "idempotentHint": false,
  "openWorldHint": true
}`

// ShellExecDeps are the dependencies shell_exec's handler needs beyond its
// input: the dispatch bridge to reach the target agent, the audit logger,
// and the elicitation-skip/timeout configuration.
type ShellExecDeps struct {
	Bridge      ShellDispatcher
	Audit       *audit.Logger
	SkipConfirm bool
	// ElicitationTimeout <= 0 uses transport.DefaultElicitationTimeout.
	ElicitationTimeout time.Duration
	// Policy, if non-nil, is checked against the command before any
	// elicitation or dispatch (RC_SHELL_ALLOWLIST/RC_SHELL_DENYLIST,
	// Section 19). A nil Policy allows everything, matching the
	// unrestricted MVP default.
	Policy *shellpolicy.Policy
}

// NewShellExecDefinition builds the shell_exec tool Definition per
// docs/specs/backend.md Section 3.1.1.
func NewShellExecDefinition(deps ShellExecDeps) Definition {
	return Definition{
		Name:               "shell_exec",
		Title:              "Execute Shell Command",
		Description:        "Run a one-shot shell command on the target device, capture stdout/stderr/exit code.",
		InputSchema:        json.RawMessage(shellExecInputSchema),
		OutputSchema:       json.RawMessage(shellExecOutputSchema),
		Annotations:        json.RawMessage(shellExecAnnotations),
		RequiredCapability: "shell",
		Handler:            deps.handle,
	}
}

func (d ShellExecDeps) handle(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.ShellExecInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if strings.TrimSpace(input.Command) == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"command\" is required"}
	}
	if input.Timeout != nil && (*input.Timeout < 1 || *input.Timeout > 300) {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"timeout\" must be between 1 and 300"}
	}

	start := time.Now()

	if allowed, reason := d.Policy.Check(input.Command); !allowed {
		d.logAudit(sess.ID, input, audit.StatusBlocked, time.Since(start), reason)
		return toolError("Command " + reason), nil
	}

	if !d.SkipConfirm {
		if declined := d.confirm(ctx, sess, input); declined != nil {
			d.logAudit(sess.ID, input, audit.StatusCancelled, time.Since(start), "declined")
			return declined, nil
		}
	}

	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}

	onProgress := agent.EmitProgress(sess, meta.ProgressToken, agent.DefaultEmitBackpressure)
	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, input.ClientID, correlationID, "shell_exec", sess.ID, input, onProgress)
	duration := time.Since(start)

	if dispatchErr != nil {
		msg := dispatchErrorMessage(dispatchErr, input.ClientID)
		d.logAudit(sess.ID, input, audit.StatusError, duration, dispatchErr.Error())
		return toolError(msg), nil
	}

	if resultPayload.IsError {
		d.logAudit(sess.ID, input, audit.StatusError, duration, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}

	output, err := decodePayload[types.ShellExecOutput](resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, input, audit.StatusError, duration, err.Error())
		return toolError("failed to decode agent result"), nil
	}
	output.ClientID = input.ClientID

	d.logAudit(sess.ID, input, audit.StatusOK, duration, "")

	outputJSON, err := json.Marshal(output)
	if err != nil {
		return toolError("failed to encode result"), nil
	}
	return &transport.ToolCallResult{
		Content:           []transport.ToolContent{{Type: "text", Text: string(outputJSON)}},
		StructuredContent: outputJSON,
	}, nil
}

// confirm runs the elicitation flow. It returns a non-nil ToolCallResult
// (the "declined" result to return to the caller) if the operation should
// not proceed, or nil if the user confirmed and dispatch may proceed.
func (d ShellExecDeps) confirm(ctx context.Context, sess *session.Session, input types.ShellExecInput) *transport.ToolCallResult {
	message := fmt.Sprintf("Execute %q on device %s?", input.Command, input.ClientID)
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"confirm": map[string]any{
				"type":        "boolean",
				"description": fmt.Sprintf("Execute this command on device %s?", input.ClientID),
			},
		},
		"required": []string{"confirm"},
	})
	if err != nil {
		return declinedResult("internal_error")
	}

	result := transport.RequestElicitation(ctx, sess, message, schema, d.ElicitationTimeout)
	if result.Declined {
		return declinedResult(result.Reason)
	}

	var confirmPayload struct {
		Confirm bool `json:"confirm"`
	}
	_ = json.Unmarshal(result.Content, &confirmPayload)
	if !confirmPayload.Confirm {
		return declinedResult("declined")
	}
	return nil
}

func (d ShellExecDeps) logAudit(sessionID string, input types.ShellExecInput, status string, duration time.Duration, errMsg string) {
	if d.Audit == nil {
		return
	}
	_ = d.Audit.LogCall(sessionID, input.ClientID, "shell_exec", input, status, duration, errMsg)
}

func dispatchErrorMessage(err error, clientID string) string {
	switch {
	case errors.Is(err, agent.ErrDeviceOffline):
		return fmt.Sprintf("Device %s is offline", clientID)
	case errors.Is(err, agent.ErrConnectionClosed):
		return "Agent disconnected during operation"
	default:
		return err.Error()
	}
}

func declinedResult(reason string) *transport.ToolCallResult {
	payload := map[string]any{"declined": true}
	if reason != "" && reason != "declined" {
		payload["reason"] = reason
	}
	data, _ := json.Marshal(payload)
	// Human-readable text per Section 13 ("Elicitation declined" /
	// "Elicitation timeout" rows); the structured payload keeps the
	// machine-readable reason.
	text := "Operation declined by user"
	if reason == "elicitation_timeout" {
		text = "Confirmation timed out"
	}
	return &transport.ToolCallResult{
		Content:           []transport.ToolContent{{Type: "text", Text: text}},
		StructuredContent: data,
	}
}

func newCorrelationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// decodePayload converts an any-typed value (already json-decoded into a
// map[string]any/etc, e.g. from a wire Envelope's Payload) into a concrete
// struct.
func decodePayload[T any](v any) (T, error) {
	var out T
	raw, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}
