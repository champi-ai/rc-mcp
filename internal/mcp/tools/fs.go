package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/audit"
	"github.com/champi-ai/rc-mcp/internal/fsroot"
	"github.com/champi-ai/rc-mcp/internal/mcp/types"
	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

const fsReadInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "path":     { "type": "string" },
    "offset":   { "type": "integer", "description": "Byte offset to start reading (default: 0)", "minimum": 0 },
    "limit":    { "type": "integer", "description": "Max bytes to read (default: 1048576 = 1MB)", "minimum": 1 },
    "encoding": { "type": "string", "enum": ["utf8", "base64"], "description": "default: utf8; falls back to base64 if not valid UTF-8" }
  },
  "required": ["clientId", "path"]
}`

const fsReadOutputSchema = `{
  "type": "object",
  "properties": {
    "content": { "type": "string" }, "encoding": { "type": "string" },
    "size": { "type": "integer" }, "truncated": { "type": "boolean" }, "clientId": { "type": "string" }
  },
  "required": ["content", "encoding", "size", "truncated", "clientId"]
}`

const fsWriteInputSchema = `{
  "type": "object",
  "properties": {
    "clientId":   { "type": "string", "description": "Target device ID" },
    "path":       { "type": "string" },
    "content":    { "type": "string" },
    "encoding":   { "type": "string", "enum": ["utf8", "base64"], "description": "default: utf8" },
    "mode":       { "type": "string", "enum": ["overwrite", "append"], "description": "default: overwrite" },
    "fileMode":   { "type": "string", "description": "Unix file mode as octal string (default: 0644)" },
    "createDirs": { "type": "boolean", "description": "Create parent directories (default: true)" }
  },
  "required": ["clientId", "path", "content"]
}`

const fsWriteOutputSchema = `{
  "type": "object",
  "properties": { "bytesWritten": { "type": "integer" }, "path": { "type": "string" }, "clientId": { "type": "string" } },
  "required": ["bytesWritten", "path", "clientId"]
}`

const fsListInputSchema = `{
  "type": "object",
  "properties": {
    "clientId":   { "type": "string", "description": "Target device ID" },
    "path":       { "type": "string" },
    "recursive":  { "type": "boolean", "description": "default: false" },
    "maxDepth":   { "type": "integer", "description": "Max recursion depth (default: 3)", "minimum": 1, "maximum": 10 },
    "showHidden": { "type": "boolean", "description": "Include dotfiles (default: false)" },
    "limit":      { "type": "integer", "description": "Max entries to return (default: 1000)", "minimum": 1 }
  },
  "required": ["clientId", "path"]
}`

const fsListOutputSchema = `{
  "type": "object",
  "properties": { "entries": { "type": "array" }, "truncated": { "type": "boolean" }, "totalCount": { "type": "integer" }, "clientId": { "type": "string" } },
  "required": ["entries", "truncated", "clientId"]
}`

const fsDeleteInputSchema = `{
  "type": "object",
  "properties": {
    "clientId":  { "type": "string", "description": "Target device ID" },
    "path":      { "type": "string" },
    "recursive": { "type": "boolean", "description": "Required for non-empty directories (default: false)" }
  },
  "required": ["clientId", "path"]
}`

const fsDeleteOutputSchema = `{
  "type": "object",
  "properties": { "deleted": { "type": "boolean" }, "path": { "type": "string" }, "itemsRemoved": { "type": "integer" }, "clientId": { "type": "string" } },
  "required": ["deleted", "path", "clientId"]
}`

const fsStatInputSchema = `{
  "type": "object",
  "properties": {
    "clientId":       { "type": "string", "description": "Target device ID" },
    "path":           { "type": "string" },
    "followSymlinks": { "type": "boolean", "description": "default: true" }
  },
  "required": ["clientId", "path"]
}`

const fsStatOutputSchema = `{
  "type": "object",
  "properties": {
    "name": { "type": "string" }, "path": { "type": "string" }, "type": { "type": "string" },
    "size": { "type": "integer" }, "mode": { "type": "string" }, "modTime": { "type": "string" },
    "owner": { "type": "string" }, "group": { "type": "string" }, "linkTarget": { "type": "string" },
    "clientId": { "type": "string" }
  },
  "required": ["name", "path", "type", "size", "mode", "modTime", "clientId"]
}`

const fsReadOnlyAnnotations = `{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}`
const fsWriteAnnotations = `{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false}`
const fsDeleteAnnotations = `{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false}`

// FSDeps are the dependencies fs_read/write/list/delete/stat handlers need.
type FSDeps struct {
	Bridge      ShellDispatcher
	Audit       *audit.Logger
	SkipConfirm bool
	// ElicitationTimeout <= 0 uses transport.DefaultElicitationTimeout.
	ElicitationTimeout time.Duration
	// GlobalRoots, if non-nil, is checked against every path before
	// elicitation or dispatch (RC_GLOBAL_FS_ALLOWED_ROOTS, Section 12.6),
	// in addition to whatever AGENT_FS_ALLOWED_ROOTS the target agent
	// itself enforces. A nil GlobalRoots allows everything.
	GlobalRoots *fsroot.Policy
}

// checkGlobalRoot returns a non-nil blocking ToolCallResult (and audits it
// as StatusBlocked) if path falls outside GlobalRoots, or nil if the call
// may proceed.
func (d FSDeps) checkGlobalRoot(sess *session.Session, tool, clientID, path string, start time.Time, input any) *transport.ToolCallResult {
	if allowed, reason := d.GlobalRoots.Check(path); !allowed {
		d.logAudit(sess.ID, clientID, tool, input, audit.StatusBlocked, time.Since(start), reason)
		return toolError("Path " + reason)
	}
	return nil
}

func NewFSReadDefinition(deps FSDeps) Definition {
	return Definition{
		Name: "fs_read", Title: "Read File",
		Description:        "Read the contents of a file on the target device. Returns text content or base64 for binary.",
		InputSchema:        json.RawMessage(fsReadInputSchema),
		OutputSchema:       json.RawMessage(fsReadOutputSchema),
		Annotations:        json.RawMessage(fsReadOnlyAnnotations),
		RequiredCapability: "fs",
		Handler:            deps.handleRead,
	}
}

func NewFSWriteDefinition(deps FSDeps) Definition {
	return Definition{
		Name: "fs_write", Title: "Write File",
		Description:        "Write content to a file on the target device. Creates parent directories as needed.",
		InputSchema:        json.RawMessage(fsWriteInputSchema),
		OutputSchema:       json.RawMessage(fsWriteOutputSchema),
		Annotations:        json.RawMessage(fsWriteAnnotations),
		RequiredCapability: "fs",
		Handler:            deps.handleWrite,
	}
}

func NewFSListDefinition(deps FSDeps) Definition {
	return Definition{
		Name: "fs_list", Title: "List Directory",
		Description:        "List directory contents with stat info on the target device.",
		InputSchema:        json.RawMessage(fsListInputSchema),
		OutputSchema:       json.RawMessage(fsListOutputSchema),
		Annotations:        json.RawMessage(fsReadOnlyAnnotations),
		RequiredCapability: "fs",
		Handler:            deps.handleList,
	}
}

func NewFSDeleteDefinition(deps FSDeps) Definition {
	return Definition{
		Name: "fs_delete", Title: "Delete File or Directory",
		Description:        "Delete a file or directory on the target device (optionally recursive).",
		InputSchema:        json.RawMessage(fsDeleteInputSchema),
		OutputSchema:       json.RawMessage(fsDeleteOutputSchema),
		Annotations:        json.RawMessage(fsDeleteAnnotations),
		RequiredCapability: "fs",
		Handler:            deps.handleDelete,
	}
}

func NewFSStatDefinition(deps FSDeps) Definition {
	return Definition{
		Name: "fs_stat", Title: "File/Directory Stat",
		Description:        "Get detailed metadata about a file or directory on the target device.",
		InputSchema:        json.RawMessage(fsStatInputSchema),
		OutputSchema:       json.RawMessage(fsStatOutputSchema),
		Annotations:        json.RawMessage(fsReadOnlyAnnotations),
		RequiredCapability: "fs",
		Handler:            deps.handleStat,
	}
}

func (d FSDeps) handleRead(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.FSReadInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" || strings.TrimSpace(input.Path) == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" and \"path\" are required"}
	}

	start := time.Now()
	if blocked := d.checkGlobalRoot(sess, "fs_read", input.ClientID, input.Path, start, input); blocked != nil {
		return blocked, nil
	}
	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}

	var streamed []byte
	onProgress := func(payload *protocol.ProgressPayload, binary *agent.BinaryFrame) {
		if binary != nil && binary.Header.FrameType == protocol.FrameFileContent {
			streamed = append(streamed, binary.Data...)
		}
	}

	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, input.ClientID, correlationID, "fs_read", sess.ID, input, onProgress)
	duration := time.Since(start)

	if dispatchErr != nil {
		msg := dispatchErrorMessage(dispatchErr, input.ClientID)
		d.logAudit(sess.ID, input.ClientID, "fs_read", input, audit.StatusError, duration, dispatchErr.Error())
		return toolError(msg), nil
	}
	if resultPayload.IsError {
		d.logAudit(sess.ID, input.ClientID, "fs_read", input, audit.StatusError, duration, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}

	output, err := decodePayload[types.FSReadOutput](resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, input.ClientID, "fs_read", input, audit.StatusError, duration, err.Error())
		return toolError("failed to decode agent result"), nil
	}
	output.ClientID = input.ClientID
	if output.Content == "" && len(streamed) > 0 {
		// Large file: the agent streamed content as binary FrameFileContent
		// frames instead of inlining it (Section 3.3.1). Assemble it here.
		if output.Encoding == "base64" {
			output.Content = base64.StdEncoding.EncodeToString(streamed)
		} else {
			output.Content = string(streamed)
		}
	}

	d.logAudit(sess.ID, input.ClientID, "fs_read", input, audit.StatusOK, duration, "")
	return marshalToolResult(output)
}

func (d FSDeps) handleWrite(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.FSWriteInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" || strings.TrimSpace(input.Path) == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" and \"path\" are required"}
	}

	start := time.Now()
	if blocked := d.checkGlobalRoot(sess, "fs_write", input.ClientID, input.Path, start, input); blocked != nil {
		return blocked, nil
	}

	if !d.SkipConfirm {
		if exists := d.fileExists(ctx, sess, input.ClientID, input.Path); exists {
			message := fmt.Sprintf("Overwrite existing file %s on device %s?", input.Path, input.ClientID)
			schema, _ := json.Marshal(map[string]any{
				"type":       "object",
				"properties": map[string]any{"confirm": map[string]any{"type": "boolean", "description": message}},
				"required":   []string{"confirm"},
			})
			result := transport.RequestElicitation(ctx, sess, message, schema, d.ElicitationTimeout)
			if result.Declined {
				d.logAudit(sess.ID, input.ClientID, "fs_write", input, audit.StatusCancelled, time.Since(start), "declined")
				return declinedResult(result.Reason), nil
			}
			var confirmPayload struct {
				Confirm bool `json:"confirm"`
			}
			_ = json.Unmarshal(result.Content, &confirmPayload)
			if !confirmPayload.Confirm {
				d.logAudit(sess.ID, input.ClientID, "fs_write", input, audit.StatusCancelled, time.Since(start), "declined")
				return declinedResult("declined"), nil
			}
		}
	}

	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}
	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, input.ClientID, correlationID, "fs_write", sess.ID, input, nil)
	duration := time.Since(start)

	if dispatchErr != nil {
		msg := dispatchErrorMessage(dispatchErr, input.ClientID)
		d.logAudit(sess.ID, input.ClientID, "fs_write", input, audit.StatusError, duration, dispatchErr.Error())
		return toolError(msg), nil
	}
	if resultPayload.IsError {
		d.logAudit(sess.ID, input.ClientID, "fs_write", input, audit.StatusError, duration, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}
	output, err := decodePayload[types.FSWriteOutput](resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, input.ClientID, "fs_write", input, audit.StatusError, duration, err.Error())
		return toolError("failed to decode agent result"), nil
	}
	output.ClientID = input.ClientID
	d.logAudit(sess.ID, input.ClientID, "fs_write", input, audit.StatusOK, duration, "")
	return marshalToolResult(output)
}

// fileExists performs the fs_write pre-check dispatch (fs_stat) so the
// caller knows whether to require overwrite confirmation. Any dispatch
// failure is treated as "does not exist" (fs_write will surface the real
// error itself if something else is actually wrong).
func (d FSDeps) fileExists(ctx context.Context, sess *session.Session, clientID, path string) bool {
	correlationID, err := newCorrelationID()
	if err != nil {
		return false
	}
	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, clientID, correlationID, "fs_stat", sess.ID, types.FSStatInput{ClientID: clientID, Path: path}, nil)
	if dispatchErr != nil || resultPayload.IsError {
		return false
	}
	return true
}

func (d FSDeps) handleList(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.FSListInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" || strings.TrimSpace(input.Path) == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" and \"path\" are required"}
	}
	if blocked := d.checkGlobalRoot(sess, "fs_list", input.ClientID, input.Path, time.Now(), input); blocked != nil {
		return blocked, nil
	}

	return d.dispatchSynchronous(ctx, sess, "fs_list", input.ClientID, input, func(payload any) (any, error) {
		out, err := decodePayload[types.FSListOutput](payload)
		if err != nil {
			return nil, err
		}
		out.ClientID = input.ClientID
		return out, nil
	})
}

func (d FSDeps) handleDelete(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.FSDeleteInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" || strings.TrimSpace(input.Path) == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" and \"path\" are required"}
	}

	start := time.Now()
	if blocked := d.checkGlobalRoot(sess, "fs_delete", input.ClientID, input.Path, start, input); blocked != nil {
		return blocked, nil
	}

	// fs_delete always requires confirmation (cannot be skipped), and the
	// user must type the exact path back as confirmPath -- a stronger
	// guard than a plain yes/no given this is irreversible.
	message := fmt.Sprintf("Delete %s on device %s? Type the path to confirm.", input.Path, input.ClientID)
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"confirmPath": map[string]any{"type": "string", "description": message},
		},
		"required": []string{"confirmPath"},
	})
	result := transport.RequestElicitation(ctx, sess, message, schema, d.ElicitationTimeout)
	if result.Declined {
		d.logAudit(sess.ID, input.ClientID, "fs_delete", input, audit.StatusCancelled, time.Since(start), "declined")
		return declinedResult(result.Reason), nil
	}
	var confirmPayload struct {
		ConfirmPath string `json:"confirmPath"`
	}
	_ = json.Unmarshal(result.Content, &confirmPayload)
	if confirmPayload.ConfirmPath != input.Path {
		d.logAudit(sess.ID, input.ClientID, "fs_delete", input, audit.StatusCancelled, time.Since(start), "confirm_path_mismatch")
		return declinedResult("confirm_path_mismatch"), nil
	}

	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}
	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, input.ClientID, correlationID, "fs_delete", sess.ID, input, nil)
	duration := time.Since(start)

	if dispatchErr != nil {
		msg := dispatchErrorMessage(dispatchErr, input.ClientID)
		d.logAudit(sess.ID, input.ClientID, "fs_delete", input, audit.StatusError, duration, dispatchErr.Error())
		return toolError(msg), nil
	}
	if resultPayload.IsError {
		d.logAudit(sess.ID, input.ClientID, "fs_delete", input, audit.StatusError, duration, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}
	output, err := decodePayload[types.FSDeleteOutput](resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, input.ClientID, "fs_delete", input, audit.StatusError, duration, err.Error())
		return toolError("failed to decode agent result"), nil
	}
	output.ClientID = input.ClientID
	d.logAudit(sess.ID, input.ClientID, "fs_delete", input, audit.StatusOK, duration, "")
	return marshalToolResult(output)
}

func (d FSDeps) handleStat(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.FSStatInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if input.ClientID == "" || strings.TrimSpace(input.Path) == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" and \"path\" are required"}
	}
	if blocked := d.checkGlobalRoot(sess, "fs_stat", input.ClientID, input.Path, time.Now(), input); blocked != nil {
		return blocked, nil
	}

	return d.dispatchSynchronous(ctx, sess, "fs_stat", input.ClientID, input, func(payload any) (any, error) {
		out, err := decodePayload[types.FSStatOutput](payload)
		if err != nil {
			return nil, err
		}
		out.ClientID = input.ClientID
		return out, nil
	})
}

func (d FSDeps) dispatchSynchronous(ctx context.Context, sess *session.Session, tool, clientID string, input any, decode func(any) (any, error)) (*transport.ToolCallResult, *transport.RPCError) {
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
	return marshalToolResult(output)
}

func (d FSDeps) logAudit(sessionID, clientID, tool string, input any, status string, duration time.Duration, errMsg string) {
	if d.Audit == nil {
		return
	}
	_ = d.Audit.LogCall(sessionID, clientID, tool, input, status, duration, errMsg)
}

func marshalToolResult(output any) (*transport.ToolCallResult, *transport.RPCError) {
	outputJSON, err := json.Marshal(output)
	if err != nil {
		return toolError("failed to encode result"), nil
	}
	return &transport.ToolCallResult{
		Content:           []transport.ToolContent{{Type: "text", Text: string(outputJSON)}},
		StructuredContent: outputJSON,
	}, nil
}
