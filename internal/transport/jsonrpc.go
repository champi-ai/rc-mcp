package transport

import (
	"encoding/json"
	"net/http"
)

// JSON-RPC error codes from docs/specs/backend.md Section 13 ("Error
// taxonomy").
const (
	codeParseError      = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeInternalError   = -32603
	codeSessionNotFound = -32001
	codeAuthFailure     = -32002
	codeRateLimited     = -32000
)

// rpcRequest is the subset of a JSON-RPC 2.0 message this transport cares
// about. A message with Method set is a request or notification; a message
// with no Method but an ID is a response to a server-initiated request
// (e.g. an elicitation/create the client is answering).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

func writeJSONRPCResult(w http.ResponseWriter, status int, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeJSONRPCError(w http.ResponseWriter, status int, id any, code int, message string) {
	writeJSONRPCErrorData(w, status, id, code, message, nil)
}

func writeJSONRPCErrorData(w http.ResponseWriter, status int, id any, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}
