package tools

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/champi-ai/rc-mcp/internal/audit"
	"github.com/champi-ai/rc-mcp/internal/mcp/types"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

const sysinfoGetInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "sections": {
      "type": "array",
      "items": { "type": "string", "enum": ["cpu", "memory", "disk", "network", "os", "uptime", "hostname", "all"] },
      "description": "Which sections to include (default: [\"all\"])"
    }
  },
  "required": ["clientId"]
}`

const sysinfoGetOutputSchema = `{
  "type": "object",
  "properties": {
    "hostname": { "type": "string" },
    "os":       { "type": "object" },
    "uptime":   { "type": "object" },
    "cpu":      { "type": "object" },
    "memory":   { "type": "object" },
    "disk":     { "type": "array" },
    "network":  { "type": "array" },
    "clientId": { "type": "string" }
  }
}`

const sysinfoGetAnnotations = `{
  "readOnlyHint": true,
  "destructiveHint": false,
  "idempotentHint": true,
  "openWorldHint": false
}`

// SysinfoDeps are the dependencies sysinfo_get's handler needs.
type SysinfoDeps struct {
	Bridge ShellDispatcher
	Audit  *audit.Logger
}

// NewSysinfoGetDefinition builds the sysinfo_get tool Definition per
// docs/specs/backend.md Section 3.5.1.
func NewSysinfoGetDefinition(deps SysinfoDeps) Definition {
	return Definition{
		Name:               "sysinfo_get",
		Title:              "Get System Information",
		Description:        "Get system overview (CPU, memory, disk, uptime, OS, hostname) from the target device.",
		InputSchema:        json.RawMessage(sysinfoGetInputSchema),
		OutputSchema:       json.RawMessage(sysinfoGetOutputSchema),
		Annotations:        json.RawMessage(sysinfoGetAnnotations),
		RequiredCapability: "sysinfo",
		Handler:            deps.handle,
	}
}

func (d SysinfoDeps) handle(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.SysinfoGetInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}

	start := time.Now()
	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}

	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, input.ClientID, correlationID, "sysinfo_get", sess.ID, input, nil)
	duration := time.Since(start)

	if dispatchErr != nil {
		msg := dispatchErrorMessage(dispatchErr, input.ClientID)
		d.logAudit(sess.ID, input.ClientID, duration, audit.StatusError, dispatchErr.Error())
		return toolError(msg), nil
	}
	if resultPayload.IsError {
		d.logAudit(sess.ID, input.ClientID, duration, audit.StatusError, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}

	output, err := decodePayload[types.SysinfoGetOutput](resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, input.ClientID, duration, audit.StatusError, err.Error())
		return toolError("failed to decode agent result"), nil
	}
	output.ClientID = input.ClientID

	d.logAudit(sess.ID, input.ClientID, duration, audit.StatusOK, "")

	outputJSON, err := json.Marshal(output)
	if err != nil {
		return toolError("failed to encode result"), nil
	}
	return &transport.ToolCallResult{
		Content:           []transport.ToolContent{{Type: "text", Text: string(outputJSON)}},
		StructuredContent: outputJSON,
	}, nil
}

func (d SysinfoDeps) logAudit(sessionID, clientID string, duration time.Duration, status, errMsg string) {
	if d.Audit == nil {
		return
	}
	_ = d.Audit.LogCall(sessionID, clientID, "sysinfo_get", map[string]string{"clientId": clientID}, status, duration, errMsg)
}
