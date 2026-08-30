package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/CloudKeter/rc-mcp/internal/session"
)

// maxRequestBodyBytes bounds how much of a POST /mcp body the handler will
// read, as a basic defense against unbounded request bodies.
const maxRequestBodyBytes = 8 << 20 // 8MB

// ServerName/ServerVersion identify this server in the initialize response
// (docs/specs/backend.md Section 2).
const (
	ServerName      = "rc-mcp"
	ServerVersion   = "0.2.0"
	ProtocolVersion = "2025-03-26"
)

// SessionIDHeader is the header used to carry the MCP session identifier.
const SessionIDHeader = "Mcp-Session-Id"

// Handler implements the Streamable HTTP MCP transport: POST/GET/DELETE
// /mcp. See docs/specs/backend.md Section 2.
type Handler struct {
	Store session.SessionStore
	Tools ToolRegistry

	// Resources, if non-nil, serves resources/list, resources/read,
	// resources/subscribe, and resources/unsubscribe (Section 4).
	Resources ResourceRegistry

	// Prompts, if non-nil, serves prompts/list and prompts/get (Section 5).
	Prompts PromptRegistry

	// Completions, if non-nil, serves completion/complete (Section 2).
	Completions CompletionRegistry

	// RateLimit, if non-nil, enforces per-session request/minute,
	// tool-call/minute, and concurrent-dispatch limits (Section 12.5).
	RateLimit *RateLimiter
}

// NewHandler constructs a Handler. tools may be nil (tools/list is then
// empty and tools/call always fails with "method not found").
func NewHandler(store session.SessionStore, tools ToolRegistry) *Handler {
	if tools == nil {
		tools = emptyRegistry{}
	}
	return &Handler{Store: store, Tools: tools}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r)
	case http.MethodGet:
		h.handleGet(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, codeParseError, "failed to read request body")
		return
	}
	if len(body) > maxRequestBodyBytes {
		writeJSONRPCError(w, http.StatusRequestEntityTooLarge, nil, codeInvalidRequest, "request body too large")
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, codeParseError, "Parse error")
		return
	}

	sessionIDHeader := r.Header.Get(SessionIDHeader)

	if req.Method == "initialize" {
		h.handleInitialize(w, req)
		return
	}

	if sessionIDHeader == "" {
		writeJSONRPCError(w, http.StatusNotFound, req.ID, codeSessionNotFound, "Mcp-Session-Id header required")
		return
	}
	sess, err := h.Store.Get(r.Context(), sessionIDHeader)
	if err != nil {
		writeJSONRPCError(w, http.StatusNotFound, req.ID, codeSessionNotFound, "Session not found")
		return
	}
	_ = h.Store.Touch(r.Context(), sess.ID)

	if h.RateLimit != nil && !h.RateLimit.AllowRequest(sess.ID) {
		writeJSONRPCError(w, http.StatusOK, req.ID, codeRateLimited, "rate limit exceeded")
		return
	}

	// A message with no "method" is a response the client is sending to a
	// server-initiated request (elicitation/create today).
	if req.Method == "" {
		h.handleClientResponse(w, sess, body, req)
		return
	}

	switch req.Method {
	case "tools/list":
		h.handleToolsList(w, r.Context(), sess, req)
	case "tools/call":
		h.handleToolsCall(w, r, sess, req)
	case "resources/list":
		h.handleResourcesList(w, r.Context(), req)
	case "resources/read":
		h.handleResourcesRead(w, r.Context(), sess, req)
	case "resources/subscribe":
		h.handleResourcesSubscribe(w, r.Context(), sess, req, true)
	case "resources/unsubscribe":
		h.handleResourcesSubscribe(w, r.Context(), sess, req, false)
	case "prompts/list":
		h.handlePromptsList(w, r.Context(), req)
	case "prompts/get":
		h.handlePromptsGet(w, r.Context(), sess, req)
	case "completion/complete":
		h.handleCompletionComplete(w, r.Context(), sess, req)
	case "notifications/cancelled":
		h.handleCancelled(w, sess, req)
	default:
		writeJSONRPCError(w, http.StatusOK, req.ID, codeMethodNotFound, fmt.Sprintf("unknown method %q", req.Method))
	}
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

func (h *Handler) handleInitialize(w http.ResponseWriter, req rpcRequest) {
	var params initializeParams
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}

	sess, err := h.Store.Create(context.Background())
	if err != nil {
		writeJSONRPCError(w, http.StatusInternalServerError, req.ID, codeInternalError, "failed to create session")
		return
	}
	sess.NegotiatedVersion = ProtocolVersion
	sess.ClientInfo = session.ClientInfo{Name: params.ClientInfo.Name, Version: params.ClientInfo.Version}

	w.Header().Set(SessionIDHeader, sess.ID)
	writeJSONRPCResult(w, http.StatusOK, req.ID, map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"tools":       map[string]any{"listChanged": true},
			"resources":   map[string]any{"subscribe": true, "listChanged": true},
			"prompts":     map[string]any{"listChanged": true},
			"logging":     map[string]any{},
			"completions": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    ServerName,
			"version": ServerVersion,
		},
	})
}

func (h *Handler) handleToolsList(w http.ResponseWriter, ctx context.Context, sess *session.Session, req rpcRequest) {
	tools := h.Tools.ListTools(ctx)
	if tools == nil {
		tools = []ToolDescriptor{}
	}
	writeJSONRPCResult(w, http.StatusOK, req.ID, map[string]any{"tools": tools})
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      struct {
		ProgressToken any `json:"progressToken,omitempty"`
	} `json:"_meta"`
}

func (h *Handler) handleToolsCall(w http.ResponseWriter, r *http.Request, sess *session.Session, req rpcRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		writeJSONRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "invalid tools/call params")
		return
	}

	if h.RateLimit != nil {
		if !h.RateLimit.AllowToolCall(sess.ID) {
			writeJSONRPCError(w, http.StatusOK, req.ID, codeRateLimited, "rate limit exceeded")
			return
		}
		if !h.RateLimit.AcquireDispatchSlot(sess.ID) {
			writeJSONRPCError(w, http.StatusOK, req.ID, codeRateLimited, "rate limit exceeded")
			return
		}
		defer h.RateLimit.ReleaseDispatchSlot(sess.ID)
	}

	reqID := fmt.Sprintf("%v", req.ID)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sess.RegisterCancel(reqID, cancel)
	defer sess.UnregisterCancel(reqID)

	progressToken := ""
	if params.Meta.ProgressToken != nil {
		progressToken = fmt.Sprintf("%v", params.Meta.ProgressToken)
	}

	meta := ToolCallMeta{RequestID: reqID, ProgressToken: progressToken}
	result, rpcErr := h.Tools.CallTool(ctx, sess, meta, params.Name, params.Arguments)
	if rpcErr != nil {
		writeJSONRPCErrorData(w, http.StatusOK, req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}
	writeJSONRPCResult(w, http.StatusOK, req.ID, result)
}

func (h *Handler) handleCancelled(w http.ResponseWriter, sess *session.Session, req rpcRequest) {
	var params struct {
		RequestID any `json:"requestId"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	sess.Cancel(fmt.Sprintf("%v", params.RequestID))
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handleClientResponse(w http.ResponseWriter, sess *session.Session, raw []byte, req rpcRequest) {
	idStr := fmt.Sprintf("%v", req.ID)
	sess.DeliverResponse(idStr, raw)
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	sessionIDHeader := r.Header.Get(SessionIDHeader)
	if sessionIDHeader == "" {
		writeJSONRPCError(w, http.StatusNotFound, nil, codeSessionNotFound, "Mcp-Session-Id header required")
		return
	}
	sess, err := h.Store.Get(r.Context(), sessionIDHeader)
	if err != nil {
		writeJSONRPCError(w, http.StatusNotFound, nil, codeSessionNotFound, "Session not found")
		return
	}
	_ = h.Store.Touch(r.Context(), sess.ID)

	serveSSE(w, r, sess)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessionIDHeader := r.Header.Get(SessionIDHeader)
	if sessionIDHeader == "" {
		writeJSONRPCError(w, http.StatusNotFound, nil, codeSessionNotFound, "Mcp-Session-Id header required")
		return
	}
	if _, err := h.Store.Get(r.Context(), sessionIDHeader); err != nil {
		writeJSONRPCError(w, http.StatusNotFound, nil, codeSessionNotFound, "Session not found")
		return
	}
	_ = h.Store.Delete(r.Context(), sessionIDHeader)
	w.WriteHeader(http.StatusNoContent)
}
