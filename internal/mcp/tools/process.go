package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/mcp/types"
	"github.com/CloudKeter/rc-mcp/internal/session"
	"github.com/CloudKeter/rc-mcp/internal/transport"
)

const processListInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "filter":   { "type": "string", "description": "Filter by process name (substring match)" },
    "user":     { "type": "string", "description": "Filter by user" },
    "sortBy":   { "type": "string", "enum": ["pid", "cpu", "memory", "name"], "description": "default: pid" },
    "limit":    { "type": "integer", "description": "Max results (default: 100)", "minimum": 1 }
  },
  "required": ["clientId"]
}`

const processListOutputSchema = `{
  "type": "object",
  "properties": {
    "processes":  { "type": "array" },
    "totalCount": { "type": "integer" },
    "clientId":   { "type": "string" }
  },
  "required": ["processes", "totalCount", "clientId"]
}`

const processInfoInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "pid":      { "type": "integer" }
  },
  "required": ["clientId", "pid"]
}`

const processInfoOutputSchema = `{
  "type": "object",
  "properties": {
    "pid": { "type": "integer" }, "ppid": { "type": "integer" }, "name": { "type": "string" },
    "cmdline": { "type": "string" }, "exe": { "type": "string" }, "cwd": { "type": "string" },
    "user": { "type": "string" }, "state": { "type": "string" }, "threads": { "type": "integer" },
    "cpuPct": { "type": "number" }, "memPct": { "type": "number" }, "memRssKB": { "type": "integer" },
    "memVmsKB": { "type": "integer" }, "startTime": { "type": "string" }, "fds": { "type": "integer" },
    "environ": { "type": "object" }, "clientId": { "type": "string" }
  },
  "required": ["pid", "name", "state", "clientId"]
}`

const processSignalInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "pid":      { "type": "integer" },
    "signal":   { "type": "string", "enum": ["SIGTERM", "SIGKILL", "SIGHUP", "SIGINT", "SIGUSR1", "SIGUSR2", "SIGSTOP", "SIGCONT"], "description": "default: SIGTERM" }
  },
  "required": ["clientId", "pid"]
}`

const processSignalOutputSchema = `{
  "type": "object",
  "properties": {
    "signalSent": { "type": "boolean" },
    "pid":        { "type": "integer" },
    "signal":     { "type": "string" },
    "clientId":   { "type": "string" }
  },
  "required": ["signalSent", "pid", "signal", "clientId"]
}`

const processListAnnotations = `{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}`
const processInfoAnnotations = `{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}`
const processSignalAnnotations = `{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false}`

// ProcessDeps are the dependencies process_list/process_info/process_signal
// handlers need.
type ProcessDeps struct {
	Bridge      ShellDispatcher
	Audit       *audit.Logger
	SkipConfirm bool
	// ElicitationTimeout <= 0 uses transport.DefaultElicitationTimeout.
	ElicitationTimeout time.Duration
}

// NewProcessListDefinition builds the process_list tool Definition per
// docs/specs/backend.md Section 3.4.1.
func NewProcessListDefinition(deps ProcessDeps) Definition {
	return Definition{
		Name:               "process_list",
		Title:              "List Processes",
		Description:        "List running processes on the target device with key metadata.",
		InputSchema:        json.RawMessage(processListInputSchema),
		OutputSchema:       json.RawMessage(processListOutputSchema),
		Annotations:        json.RawMessage(processListAnnotations),
		RequiredCapability: "process",
		Handler:            deps.handleList,
	}
}

// NewProcessInfoDefinition builds the process_info tool Definition per
// Section 3.4.2.
func NewProcessInfoDefinition(deps ProcessDeps) Definition {
	return Definition{
		Name:               "process_info",
		Title:              "Get Process Info",
		Description:        "Get detailed information about a specific process on the target device.",
		InputSchema:        json.RawMessage(processInfoInputSchema),
		OutputSchema:       json.RawMessage(processInfoOutputSchema),
		Annotations:        json.RawMessage(processInfoAnnotations),
		RequiredCapability: "process",
		Handler:            deps.handleInfo,
	}
}

// NewProcessSignalDefinition builds the process_signal tool Definition per
// Section 3.4.3.
func NewProcessSignalDefinition(deps ProcessDeps) Definition {
	return Definition{
		Name:               "process_signal",
		Title:              "Send Signal to Process",
		Description:        "Send a Unix signal to a process on the target device.",
		InputSchema:        json.RawMessage(processSignalInputSchema),
		OutputSchema:       json.RawMessage(processSignalOutputSchema),
		Annotations:        json.RawMessage(processSignalAnnotations),
		RequiredCapability: "process",
		Handler:            deps.handleSignal,
	}
}

func (d ProcessDeps) handleList(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.ProcessListInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}

	return d.dispatchSynchronous(ctx, sess, "process_list", input.ClientID, input, func(payload any) (any, error) {
		out, err := decodePayload[types.ProcessListOutput](payload)
		if err != nil {
			return nil, err
		}
		out.ClientID = input.ClientID
		return out, nil
	})
}

func (d ProcessDeps) handleInfo(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.ProcessInfoInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}

	return d.dispatchSynchronous(ctx, sess, "process_info", input.ClientID, input, func(payload any) (any, error) {
		out, err := decodePayload[types.ProcessInfoOutput](payload)
		if err != nil {
			return nil, err
		}
		out.ClientID = input.ClientID
		return out, nil
	})
}

func (d ProcessDeps) handleSignal(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.ProcessSignalInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}

	start := time.Now()

	if !d.SkipConfirm {
		signal := "SIGTERM"
		if input.Signal != nil && *input.Signal != "" {
			signal = *input.Signal
		}
		message := fmt.Sprintf("Send %s to process %d on device %s?", signal, input.PID, input.ClientID)
		schema, err := json.Marshal(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{"type": "boolean", "description": message},
			},
			"required": []string{"confirm"},
		})
		if err != nil {
			return declinedResult("internal_error"), nil
		}
		result := transport.RequestElicitation(ctx, sess, message, schema, d.ElicitationTimeout)
		if result.Declined {
			d.logAudit(sess.ID, input.ClientID, "process_signal", input, audit.StatusCancelled, time.Since(start), "declined")
			return declinedResult(result.Reason), nil
		}
		var confirmPayload struct {
			Confirm bool `json:"confirm"`
		}
		_ = json.Unmarshal(result.Content, &confirmPayload)
		if !confirmPayload.Confirm {
			d.logAudit(sess.ID, input.ClientID, "process_signal", input, audit.StatusCancelled, time.Since(start), "declined")
			return declinedResult("declined"), nil
		}
	}

	return d.dispatchSynchronous(ctx, sess, "process_signal", input.ClientID, input, func(payload any) (any, error) {
		out, err := decodePayload[types.ProcessSignalOutput](payload)
		if err != nil {
			return nil, err
		}
		out.ClientID = input.ClientID
		return out, nil
	})
}

// dispatchSynchronous is the common path shared by all three process tools:
// mint a correlation ID, dispatch, decode the agent's result via decode,
// and audit-log the outcome.
func (d ProcessDeps) dispatchSynchronous(ctx context.Context, sess *session.Session, tool, clientID string, input any, decode func(any) (any, error)) (*transport.ToolCallResult, *transport.RPCError) {
	start := time.Now()
	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}

	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, clientID, correlationID, tool, sess.ID, input, nil)
	duration := time.Since(start)

	if dispatchErr != nil {
		msg := dispatchErrorMessage(dispatchErr, clientID)
		d.logAudit(sess.ID, clientID, tool, input, audit.StatusError, duration, dispatchErr.Error())
		return toolError(msg), nil
	}
	if resultPayload.IsError {
		d.logAudit(sess.ID, clientID, tool, input, audit.StatusError, duration, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}

	output, err := decode(resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, clientID, tool, input, audit.StatusError, duration, err.Error())
		return toolError("failed to decode agent result"), nil
	}
	d.logAudit(sess.ID, clientID, tool, input, audit.StatusOK, duration, "")

	outputJSON, err := json.Marshal(output)
	if err != nil {
		return toolError("failed to encode result"), nil
	}
	return &transport.ToolCallResult{
		Content:           []transport.ToolContent{{Type: "text", Text: string(outputJSON)}},
		StructuredContent: outputJSON,
	}, nil
}

func (d ProcessDeps) logAudit(sessionID, clientID, tool string, input any, status string, duration time.Duration, errMsg string) {
	if d.Audit == nil {
		return
	}
	_ = d.Audit.LogCall(sessionID, clientID, tool, input, status, duration, errMsg)
}
