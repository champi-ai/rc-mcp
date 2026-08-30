package transport

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/CloudKeter/rc-mcp/internal/session"
)

// PromptArgument describes one argument of a prompt (prompts/list).
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptDescriptor is one entry of a prompts/list result.
type PromptDescriptor struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptContent is the content of one prompt message (text only; the
// Section 5 prompts emit no other content types).
type PromptContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// PromptMessage is one message of a prompts/get result.
type PromptMessage struct {
	Role    string        `json:"role"`
	Content PromptContent `json:"content"`
}

// PromptResult is a prompts/get result.
type PromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// PromptRegistry is implemented by internal/mcp/prompts.Registry and wired
// into the transport Handler by cmd/server (like ToolRegistry).
type PromptRegistry interface {
	// ListPrompts returns the prompts visible to prompts/list.
	ListPrompts(ctx context.Context) []PromptDescriptor
	// GetPrompt renders name with args (Section 5's dynamic templates may
	// dispatch to the target agent, e.g. a sysinfo snapshot).
	GetPrompt(ctx context.Context, sess *session.Session, name string, args map[string]string) (*PromptResult, *RPCError)
}

type promptGetParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments"`
}

func (h *Handler) handlePromptsList(w http.ResponseWriter, ctx context.Context, req rpcRequest) {
	if h.Prompts == nil {
		writeJSONRPCResult(w, http.StatusOK, req.ID, map[string]any{"prompts": []PromptDescriptor{}})
		return
	}
	prompts := h.Prompts.ListPrompts(ctx)
	if prompts == nil {
		prompts = []PromptDescriptor{}
	}
	writeJSONRPCResult(w, http.StatusOK, req.ID, map[string]any{"prompts": prompts})
}

func (h *Handler) handlePromptsGet(w http.ResponseWriter, ctx context.Context, sess *session.Session, req rpcRequest) {
	if h.Prompts == nil {
		writeJSONRPCError(w, http.StatusOK, req.ID, codeMethodNotFound, "prompts are not available")
		return
	}
	var params promptGetParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		writeJSONRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "invalid params: \"name\" is required")
		return
	}
	result, rpcErr := h.Prompts.GetPrompt(ctx, sess, params.Name, params.Arguments)
	if rpcErr != nil {
		writeJSONRPCErrorData(w, http.StatusOK, req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}
	writeJSONRPCResult(w, http.StatusOK, req.ID, result)
}
