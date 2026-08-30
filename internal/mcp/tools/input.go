// This file implements the input_key, input_mouse_click, input_mouse_move,
// and input_type tools: the `input` capability area (docs/specs/backend.md
// Section 19). Unlike every other tool group, elicitation confirmation is
// mandatory on every single call -- there is deliberately no SkipConfirm
// field on InputDeps, and no env var can bypass it, given the
// significantly larger attack surface direct keyboard/mouse control
// represents.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/champi-ai/rc-mcp/internal/audit"
	"github.com/champi-ai/rc-mcp/internal/mcp/types"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

const inputKeyInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "key":      { "type": "string", "description": "xdotool-syntax key spec, e.g. \"Return\", \"ctrl+c\", \"F5\"" }
  },
  "required": ["clientId", "key"]
}`

const inputMouseClickInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "x":        { "type": "integer" },
    "y":        { "type": "integer" },
    "button":   { "type": "string", "enum": ["left", "middle", "right"], "description": "default: left" }
  },
  "required": ["clientId", "x", "y"]
}`

const inputMouseMoveInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "x":        { "type": "integer" },
    "y":        { "type": "integer" }
  },
  "required": ["clientId", "x", "y"]
}`

const inputTypeInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "text":     { "type": "string" }
  },
  "required": ["clientId", "text"]
}`

const inputOutputSchema = `{
  "type": "object",
  "properties": { "clientId": { "type": "string" } },
  "required": ["clientId"]
}`

const inputAnnotations = `{
  "readOnlyHint": false,
  "destructiveHint": true,
  "idempotentHint": false,
  "openWorldHint": true
}`

// InputDeps are the dependencies the input_* tool handlers need. There is
// deliberately no SkipConfirm field: every action requires elicitation
// confirmation, with no bypass configuration whatsoever.
type InputDeps struct {
	Bridge ShellDispatcher
	Audit  *audit.Logger
	// ElicitationTimeout <= 0 uses transport.DefaultElicitationTimeout.
	ElicitationTimeout time.Duration
}

func NewInputKeyDefinition(deps InputDeps) Definition {
	return Definition{
		Name:               "input_key",
		Title:              "Send Keypress",
		Description:        "Send a keypress or key combination to the target device's active window.",
		InputSchema:        json.RawMessage(inputKeyInputSchema),
		OutputSchema:       json.RawMessage(inputOutputSchema),
		Annotations:        json.RawMessage(inputAnnotations),
		RequiredCapability: "input",
		Handler:            deps.handleKey,
	}
}

func NewInputMouseClickDefinition(deps InputDeps) Definition {
	return Definition{
		Name:               "input_mouse_click",
		Title:              "Click Mouse",
		Description:        "Move the mouse to the given coordinates and click on the target device.",
		InputSchema:        json.RawMessage(inputMouseClickInputSchema),
		OutputSchema:       json.RawMessage(inputOutputSchema),
		Annotations:        json.RawMessage(inputAnnotations),
		RequiredCapability: "input",
		Handler:            deps.handleMouseClick,
	}
}

func NewInputMouseMoveDefinition(deps InputDeps) Definition {
	return Definition{
		Name:               "input_mouse_move",
		Title:              "Move Mouse",
		Description:        "Move the mouse cursor to the given coordinates on the target device, without clicking.",
		InputSchema:        json.RawMessage(inputMouseMoveInputSchema),
		OutputSchema:       json.RawMessage(inputOutputSchema),
		Annotations:        json.RawMessage(inputAnnotations),
		RequiredCapability: "input",
		Handler:            deps.handleMouseMove,
	}
}

func NewInputTypeDefinition(deps InputDeps) Definition {
	return Definition{
		Name:               "input_type",
		Title:              "Type Text",
		Description:        "Type literal text into the target device's active window.",
		InputSchema:        json.RawMessage(inputTypeInputSchema),
		OutputSchema:       json.RawMessage(inputOutputSchema),
		Annotations:        json.RawMessage(inputAnnotations),
		RequiredCapability: "input",
		Handler:            deps.handleType,
	}
}

// confirmInput runs the mandatory elicitation flow shared by every input_*
// tool. It returns a non-nil ToolCallResult (the "declined" result to
// return to the caller) if the operation should not proceed, or nil if
// the user confirmed and dispatch may proceed.
func (d InputDeps) confirmInput(ctx context.Context, sess *session.Session, clientID, action string) *transport.ToolCallResult {
	message := fmt.Sprintf("Allow %s on device %s?", action, clientID)
	schema, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": map[string]any{"confirm": map[string]any{"type": "boolean", "description": message}},
		"required":   []string{"confirm"},
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

func (d InputDeps) handleKey(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.InputKeyInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" || input.Key == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" and \"key\" are required"}
	}

	start := time.Now()
	if declined := d.confirmInput(ctx, sess, input.ClientID, fmt.Sprintf("sending key %q", input.Key)); declined != nil {
		d.logAudit(sess.ID, input.ClientID, "input_key", input, audit.StatusCancelled, time.Since(start), "declined")
		return declined, nil
	}

	return d.dispatchAndLog(ctx, sess, "input_key", input.ClientID, input, start, func(payload any) (any, error) {
		out, err := decodePayload[types.InputKeyOutput](payload)
		if err != nil {
			return nil, err
		}
		out.ClientID = input.ClientID
		return out, nil
	})
}

func (d InputDeps) handleMouseClick(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.InputMouseClickInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}

	start := time.Now()
	action := fmt.Sprintf("a mouse click at (%d, %d)", input.X, input.Y)
	if declined := d.confirmInput(ctx, sess, input.ClientID, action); declined != nil {
		d.logAudit(sess.ID, input.ClientID, "input_mouse_click", input, audit.StatusCancelled, time.Since(start), "declined")
		return declined, nil
	}

	return d.dispatchAndLog(ctx, sess, "input_mouse_click", input.ClientID, input, start, func(payload any) (any, error) {
		out, err := decodePayload[types.InputMouseClickOutput](payload)
		if err != nil {
			return nil, err
		}
		out.ClientID = input.ClientID
		return out, nil
	})
}

func (d InputDeps) handleMouseMove(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.InputMouseMoveInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}

	start := time.Now()
	action := fmt.Sprintf("moving the mouse to (%d, %d)", input.X, input.Y)
	if declined := d.confirmInput(ctx, sess, input.ClientID, action); declined != nil {
		d.logAudit(sess.ID, input.ClientID, "input_mouse_move", input, audit.StatusCancelled, time.Since(start), "declined")
		return declined, nil
	}

	return d.dispatchAndLog(ctx, sess, "input_mouse_move", input.ClientID, input, start, func(payload any) (any, error) {
		out, err := decodePayload[types.InputMouseMoveOutput](payload)
		if err != nil {
			return nil, err
		}
		out.ClientID = input.ClientID
		return out, nil
	})
}

func (d InputDeps) handleType(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.InputTypeInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}

	start := time.Now()
	if declined := d.confirmInput(ctx, sess, input.ClientID, "typing text"); declined != nil {
		d.logAudit(sess.ID, input.ClientID, "input_type", input, audit.StatusCancelled, time.Since(start), "declined")
		return declined, nil
	}

	return d.dispatchAndLog(ctx, sess, "input_type", input.ClientID, input, start, func(payload any) (any, error) {
		out, err := decodePayload[types.InputTypeOutput](payload)
		if err != nil {
			return nil, err
		}
		out.ClientID = input.ClientID
		return out, nil
	})
}

// dispatchAndLog dispatches tool to clientID, decodes the result via
// decode, and audits the outcome -- the common tail shared by all four
// input_* handlers once confirmation has already succeeded.
func (d InputDeps) dispatchAndLog(ctx context.Context, sess *session.Session, tool, clientID string, input any, start time.Time, decode func(any) (any, error)) (*transport.ToolCallResult, *transport.RPCError) {
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
	return marshalToolResult(output)
}

func (d InputDeps) logAudit(sessionID, clientID, tool string, input any, status string, duration time.Duration, errMsg string) {
	if d.Audit == nil {
		return
	}
	_ = d.Audit.LogCall(sessionID, clientID, tool, input, status, duration, errMsg)
}
