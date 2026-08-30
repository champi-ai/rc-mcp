package transport

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/champi-ai/rc-mcp/internal/session"
)

// CompletionRef identifies what is being completed: a tool name (this
// server extends the standard MCP "ref/prompt"/"ref/resource" kinds with
// "ref/tool", per docs/specs/backend.md Section 2's "Argument
// auto-completion for tool inputs"), a prompt name, or a resource URI
// template.
type CompletionRef struct {
	Type string `json:"type"` // "ref/tool" | "ref/prompt" | "ref/resource"
	Name string `json:"name,omitempty"`
	URI  string `json:"uri,omitempty"`
}

// CompletionArgument is the argument being completed and its current
// (possibly partial) value.
type CompletionArgument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CompletionContext carries already-resolved values of other arguments in
// the same call (e.g. clientId, needed to complete a "path" argument
// against the right device), matching the MCP completion/complete
// "context.arguments" shape.
type CompletionContext struct {
	Arguments map[string]string `json:"arguments,omitempty"`
}

// CompletionValues is a completion/complete result's "completion" member.
type CompletionValues struct {
	Values  []string `json:"values"`
	Total   *int     `json:"total,omitempty"`
	HasMore bool     `json:"hasMore,omitempty"`
}

// CompletionRegistry is implemented by internal/mcp/completions.Registry
// and wired into the transport Handler by cmd/server (like ToolRegistry).
type CompletionRegistry interface {
	Complete(ctx context.Context, sess *session.Session, ref CompletionRef, arg CompletionArgument, compCtx CompletionContext) (*CompletionValues, *RPCError)
}

type completeParams struct {
	Ref      CompletionRef      `json:"ref"`
	Argument CompletionArgument `json:"argument"`
	Context  CompletionContext  `json:"context"`
}

func (h *Handler) handleCompletionComplete(w http.ResponseWriter, ctx context.Context, sess *session.Session, req rpcRequest) {
	if h.Completions == nil {
		writeJSONRPCError(w, http.StatusOK, req.ID, codeMethodNotFound, "completions are not available")
		return
	}
	var params completeParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Ref.Type == "" || params.Argument.Name == "" {
		writeJSONRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "invalid params: \"ref\" and \"argument.name\" are required")
		return
	}
	result, rpcErr := h.Completions.Complete(ctx, sess, params.Ref, params.Argument, params.Context)
	if rpcErr != nil {
		writeJSONRPCErrorData(w, http.StatusOK, req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}
	if result == nil {
		result = &CompletionValues{Values: []string{}}
	}
	writeJSONRPCResult(w, http.StatusOK, req.ID, map[string]any{"completion": result})
}
