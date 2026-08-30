package transport

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/champi-ai/rc-mcp/internal/session"
)

// ResourceDescriptor is one entry of a resources/list result. URI may be a
// template (e.g. "job://{id}"); templated resources are still listed here
// so clients can discover them (Section 4).
type ResourceDescriptor struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceContent is one element of a resources/read result's "contents"
// array.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// ResourceRegistry is implemented by internal/mcp/resources.Registry and
// wired into the transport Handler by cmd/server. Declared here (like
// ToolRegistry) so the transport has no dependency on internal/mcp.
type ResourceRegistry interface {
	// ListResources returns the resources visible to resources/list.
	ListResources(ctx context.Context) []ResourceDescriptor
	// ReadResource resolves uri and returns its contents.
	ReadResource(ctx context.Context, sess *session.Session, uri string) ([]ResourceContent, *RPCError)
	// SubscribeResource registers sess for notifications/resources/updated
	// on uri (Section 4, "Subscribe: yes").
	SubscribeResource(ctx context.Context, sess *session.Session, uri string) *RPCError
	// UnsubscribeResource removes a subscription added by SubscribeResource.
	UnsubscribeResource(ctx context.Context, sess *session.Session, uri string) *RPCError
}

type resourceParams struct {
	URI string `json:"uri"`
}

func (h *Handler) handleResourcesList(w http.ResponseWriter, ctx context.Context, req rpcRequest) {
	if h.Resources == nil {
		writeJSONRPCResult(w, http.StatusOK, req.ID, map[string]any{"resources": []ResourceDescriptor{}})
		return
	}
	resources := h.Resources.ListResources(ctx)
	if resources == nil {
		resources = []ResourceDescriptor{}
	}
	writeJSONRPCResult(w, http.StatusOK, req.ID, map[string]any{"resources": resources})
}

func (h *Handler) handleResourcesRead(w http.ResponseWriter, ctx context.Context, sess *session.Session, req rpcRequest) {
	uri, ok := h.resourceURI(w, req)
	if !ok {
		return
	}
	contents, rpcErr := h.Resources.ReadResource(ctx, sess, uri)
	if rpcErr != nil {
		writeJSONRPCErrorData(w, http.StatusOK, req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}
	writeJSONRPCResult(w, http.StatusOK, req.ID, map[string]any{"contents": contents})
}

func (h *Handler) handleResourcesSubscribe(w http.ResponseWriter, ctx context.Context, sess *session.Session, req rpcRequest, subscribe bool) {
	uri, ok := h.resourceURI(w, req)
	if !ok {
		return
	}
	var rpcErr *RPCError
	if subscribe {
		rpcErr = h.Resources.SubscribeResource(ctx, sess, uri)
	} else {
		rpcErr = h.Resources.UnsubscribeResource(ctx, sess, uri)
	}
	if rpcErr != nil {
		writeJSONRPCErrorData(w, http.StatusOK, req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}
	writeJSONRPCResult(w, http.StatusOK, req.ID, map[string]any{})
}

// resourceURI extracts params.uri, writing the error response itself when
// the request is unusable (nil Resources registry or missing uri).
func (h *Handler) resourceURI(w http.ResponseWriter, req rpcRequest) (string, bool) {
	if h.Resources == nil {
		writeJSONRPCError(w, http.StatusOK, req.ID, codeMethodNotFound, "resources are not available")
		return "", false
	}
	var params resourceParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.URI == "" {
		writeJSONRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "invalid params: \"uri\" is required")
		return "", false
	}
	return params.URI, true
}
