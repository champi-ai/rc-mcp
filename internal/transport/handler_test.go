package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/session"
)

func newTestHandler() (*Handler, session.SessionStore) {
	store := session.NewMemoryStore(10)
	return NewHandler(store, nil), store
}

func doInitialize(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"test","version":"1.0"}}}`
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST initialize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", resp.StatusCode)
	}
	sid := resp.Header.Get(SessionIDHeader)
	if len(sid) != 32 {
		t.Fatalf("Mcp-Session-Id = %q, want 32 hex chars", sid)
	}
	var rpcResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	result, ok := rpcResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize response missing result: %+v", rpcResp)
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("capabilities missing tools: %+v", caps)
	}
	return sid
}

func TestHandler_InitializeIssuesSessionID(t *testing.T) {
	h, _ := newTestHandler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	doInitialize(t, srv)
}

func TestHandler_MissingSessionIDReturns404(t *testing.T) {
	h, _ := newTestHandler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandler_UnknownSessionIDReturns404(t *testing.T) {
	h, _ := newTestHandler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req.Header.Set(SessionIDHeader, "0000000000000000000000000000000000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandler_ToolsListEmptyByDefault(t *testing.T) {
	h, _ := newTestHandler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req.Header.Set(SessionIDHeader, sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	result := out["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf("tools = %v, want empty", tools)
	}
}

func TestHandler_ToolsCallUnregisteredIsMethodNotFound(t *testing.T) {
	h, _ := newTestHandler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"shell_exec","arguments":{}}}`))
	req.Header.Set(SessionIDHeader, sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error response, got %+v", out)
	}
	if int(errObj["code"].(float64)) != codeMethodNotFound {
		t.Fatalf("code = %v, want %d", errObj["code"], codeMethodNotFound)
	}
}

// stubRegistry is a minimal ToolRegistry used to test tools/call routing
// and progress emission through the session's SSE stream.
type stubRegistry struct {
	call func(ctx context.Context, sess *session.Session, meta ToolCallMeta, name string, args json.RawMessage) (*ToolCallResult, *RPCError)
}

func (s *stubRegistry) ListTools(context.Context) []ToolDescriptor {
	return []ToolDescriptor{{Name: "echo", InputSchema: json.RawMessage(`{}`)}}
}

func (s *stubRegistry) CallTool(ctx context.Context, sess *session.Session, meta ToolCallMeta, name string, args json.RawMessage) (*ToolCallResult, *RPCError) {
	return s.call(ctx, sess, meta, name, args)
}

func TestHandler_ToolsCallSuccess(t *testing.T) {
	store := session.NewMemoryStore(10)
	reg := &stubRegistry{call: func(ctx context.Context, sess *session.Session, meta ToolCallMeta, name string, args json.RawMessage) (*ToolCallResult, *RPCError) {
		return &ToolCallResult{Content: []ToolContent{{Type: "text", Text: "ok"}}}, nil
	}}
	h := NewHandler(store, reg)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	req.Header.Set(SessionIDHeader, sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result, got %+v", out)
	}
	content := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v", content)
	}
}

func TestHandler_ToolsCallRateLimited(t *testing.T) {
	store := session.NewMemoryStore(10)
	reg := &stubRegistry{call: func(ctx context.Context, sess *session.Session, meta ToolCallMeta, name string, args json.RawMessage) (*ToolCallResult, *RPCError) {
		return &ToolCallResult{Content: []ToolContent{{Type: "text", Text: "ok"}}}, nil
	}}
	h := NewHandler(store, reg)
	h.RateLimit = NewRateLimiter(1000, 1, 1000) // 1 tool call/minute
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	doToolCall := func(id int) *http.Response {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, id)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set(SessionIDHeader, sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		return resp
	}

	resp1 := doToolCall(10)
	resp1.Body.Close()

	resp2 := doToolCall(11)
	defer resp2.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected rate-limit error, got %+v", out)
	}
	if int(errObj["code"].(float64)) != codeRateLimited {
		t.Fatalf("code = %v, want %d", errObj["code"], codeRateLimited)
	}
	if errObj["message"] != "rate limit exceeded" {
		t.Fatalf("message = %v, want %q", errObj["message"], "rate limit exceeded")
	}
}

func TestHandler_ConcurrentDispatchCapRejectsRatherThanQueues(t *testing.T) {
	store := session.NewMemoryStore(10)
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	reg := &stubRegistry{call: func(ctx context.Context, sess *session.Session, meta ToolCallMeta, name string, args json.RawMessage) (*ToolCallResult, *RPCError) {
		started <- struct{}{}
		<-release
		return &ToolCallResult{Content: []ToolContent{{Type: "text", Text: "ok"}}}, nil
	}}
	h := NewHandler(store, reg)
	h.RateLimit = NewRateLimiter(1000, 1000, 1) // 1 concurrent dispatch
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	go func() {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
		req.Header.Set(SessionIDHeader, sid)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started // first call is now occupying the only slot

	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	req2.Header.Set(SessionIDHeader, sid)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp2.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected rate-limit error while at concurrency cap, got %+v", out)
	}
	if int(errObj["code"].(float64)) != codeRateLimited {
		t.Fatalf("code = %v, want %d", errObj["code"], codeRateLimited)
	}

	close(release)
}

func TestHandler_DeleteTerminatesSession(t *testing.T) {
	h, store := newTestHandler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/mcp", nil)
	req.Header.Set(SessionIDHeader, sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	if _, err := store.Get(context.Background(), sid); err == nil {
		t.Fatal("session should be removed after DELETE")
	}
}

func TestHandler_SSEStreamAndReplay(t *testing.T) {
	h, store := newTestHandler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	sess, err := store.Get(context.Background(), sid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Emit one event before the client connects; it should be delivered as
	// part of the live stream (not just replay) since Emit happens after
	// GET begins reading in this test... instead emit AFTER connecting to
	// exercise the live path, then disconnect and use Last-Event-ID for
	// replay of an event we emit while nobody is connected.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req = req.WithContext(ctx)
	req.Header.Set(SessionIDHeader, sid)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	reader := bufio.NewReader(resp.Body)

	if !sess.Emit(session.SSEEvent{Data: `{"hello":1}`}, time.Second) {
		t.Fatal("Emit failed")
	}

	line := readSSEDataLine(t, reader)
	if line != `{"hello":1}` {
		t.Fatalf("got data %q, want hello event", line)
	}

	cancel()
	resp.Body.Close()

	// Give the server a moment to notice the disconnect before we emit the
	// event we expect to be replay-only.
	time.Sleep(50 * time.Millisecond)
	if !sess.Emit(session.SSEEvent{Data: `{"missed":2}`}, time.Second) {
		t.Fatal("Emit (missed) failed")
	}
	// Nobody is reading EventCh right now (writer goroutine exited with the
	// cancelled GET), so give the backpressure path a moment; then drain it
	// manually into the replay buffer the way the SSE writer would -- but
	// since no writer is attached, the event sits in EventCh undelivered
	// and unreplayable. To exercise replay realistically, append directly
	// to the replay buffer instead, simulating what the (absent) writer
	// would have recorded.
	sess.ReplayBuffer.Append("message", `{"missed":2}`)

	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req2.Header.Set(SessionIDHeader, sid)
	req2.Header.Set("Last-Event-ID", "1")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("reconnect GET: %v", err)
	}
	defer resp2.Body.Close()
	reader2 := bufio.NewReader(resp2.Body)
	line2 := readSSEDataLine(t, reader2)
	if line2 != `{"missed":2}` {
		t.Fatalf("replay got %q, want missed event", line2)
	}
}

func TestHandler_SSEReplayTooOldReturns204(t *testing.T) {
	h, store := newTestHandler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	sess, err := store.Get(context.Background(), sid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	sess.ReplayBuffer = session.NewReplayBuffer(1)
	for i := 0; i < 5; i++ {
		sess.ReplayBuffer.Append("message", fmt.Sprintf("%d", i))
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req.Header.Set(SessionIDHeader, sid)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func readSSEDataLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			continue
		}
		line = strings.TrimRight(line, "\n")
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatal("timed out waiting for an SSE data line")
	return ""
}

func TestHandler_ToolsCallRPCErrorDataReachesWire(t *testing.T) {
	store := session.NewMemoryStore(10)
	reg := &stubRegistry{call: func(ctx context.Context, sess *session.Session, meta ToolCallMeta, name string, args json.RawMessage) (*ToolCallResult, *RPCError) {
		return nil, &RPCError{
			Code:    codeInvalidParams,
			Message: "invalid params",
			Data:    map[string]any{"validationErrors": []map[string]any{{"path": "/clientId", "message": "required field is missing"}}},
		}
	}}
	h := NewHandler(store, reg)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	req.Header.Set(SessionIDHeader, sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Error struct {
			Code int `json:"code"`
			Data struct {
				ValidationErrors []map[string]any `json:"validationErrors"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Error.Code != codeInvalidParams {
		t.Fatalf("code = %d, want %d", out.Error.Code, codeInvalidParams)
	}
	if len(out.Error.Data.ValidationErrors) != 1 || out.Error.Data.ValidationErrors[0]["path"] != "/clientId" {
		t.Fatalf("validationErrors = %+v", out.Error.Data.ValidationErrors)
	}
}

type stubResources struct{}

func (stubResources) ListResources(context.Context) []ResourceDescriptor {
	return []ResourceDescriptor{{URI: "clients://list", Name: "Paired Devices", MimeType: "application/json"}}
}
func (stubResources) ReadResource(ctx context.Context, sess *session.Session, uri string) ([]ResourceContent, *RPCError) {
	if uri != "clients://list" {
		return nil, &RPCError{Code: codeInvalidParams, Message: "unknown resource"}
	}
	return []ResourceContent{{URI: uri, MimeType: "application/json", Text: `{"clients":[]}`}}, nil
}
func (stubResources) SubscribeResource(ctx context.Context, sess *session.Session, uri string) *RPCError {
	sess.Subscribe(uri)
	return nil
}
func (stubResources) UnsubscribeResource(ctx context.Context, sess *session.Session, uri string) *RPCError {
	sess.Unsubscribe(uri)
	return nil
}

func TestHandler_ResourcesRouting(t *testing.T) {
	store := session.NewMemoryStore(10)
	h := NewHandler(store, nil)
	h.Resources = stubResources{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	post := func(body string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set(SessionIDHeader, sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	out := post(`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)
	resources := out["result"].(map[string]any)["resources"].([]any)
	if len(resources) != 1 {
		t.Fatalf("resources = %+v", resources)
	}

	out = post(`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"clients://list"}}`)
	contents := out["result"].(map[string]any)["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["text"] != `{"clients":[]}` {
		t.Fatalf("contents = %+v", contents)
	}

	out = post(`{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"bogus://x"}}`)
	if out["error"] == nil {
		t.Fatalf("want error for unknown resource, got %+v", out)
	}

	out = post(`{"jsonrpc":"2.0","id":4,"method":"resources/subscribe","params":{"uri":"clients://list"}}`)
	if out["error"] != nil {
		t.Fatalf("subscribe error: %+v", out)
	}
	sess, err := store.Get(context.Background(), sid)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if !sess.IsSubscribed("clients://list") {
		t.Fatal("session not subscribed after resources/subscribe")
	}

	out = post(`{"jsonrpc":"2.0","id":5,"method":"resources/unsubscribe","params":{"uri":"clients://list"}}`)
	if out["error"] != nil {
		t.Fatalf("unsubscribe error: %+v", out)
	}
	if sess.IsSubscribed("clients://list") {
		t.Fatal("still subscribed after resources/unsubscribe")
	}

	out = post(`{"jsonrpc":"2.0","id":6,"method":"resources/read","params":{}}`)
	if out["error"] == nil {
		t.Fatal("missing uri must error")
	}
}

type stubPrompts struct{}

func (stubPrompts) ListPrompts(context.Context) []PromptDescriptor {
	return []PromptDescriptor{{Name: "diagnose_system", Arguments: []PromptArgument{{Name: "clientId", Required: true}}}}
}
func (stubPrompts) GetPrompt(ctx context.Context, sess *session.Session, name string, args map[string]string) (*PromptResult, *RPCError) {
	if name != "diagnose_system" {
		return nil, &RPCError{Code: codeInvalidParams, Message: "unknown prompt"}
	}
	return &PromptResult{
		Description: "diag " + args["clientId"],
		Messages:    []PromptMessage{{Role: "user", Content: PromptContent{Type: "text", Text: "go"}}},
	}, nil
}

func TestHandler_PromptsRouting(t *testing.T) {
	store := session.NewMemoryStore(10)
	h := NewHandler(store, nil)
	h.Prompts = stubPrompts{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	post := func(body string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set(SessionIDHeader, sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	out := post(`{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`)
	promptsList := out["result"].(map[string]any)["prompts"].([]any)
	if len(promptsList) != 1 || promptsList[0].(map[string]any)["name"] != "diagnose_system" {
		t.Fatalf("prompts = %+v", promptsList)
	}

	out = post(`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"diagnose_system","arguments":{"clientId":"dev-1"}}}`)
	result := out["result"].(map[string]any)
	if result["description"] != "diag dev-1" {
		t.Fatalf("result = %+v", result)
	}
	messages := result["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages = %+v", messages)
	}

	if out = post(`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"nope"}}`); out["error"] == nil {
		t.Fatal("unknown prompt must error")
	}
	if out = post(`{"jsonrpc":"2.0","id":4,"method":"prompts/get","params":{}}`); out["error"] == nil {
		t.Fatal("missing name must error")
	}
}

type stubCompletions struct{}

func (stubCompletions) Complete(ctx context.Context, sess *session.Session, ref CompletionRef, arg CompletionArgument, compCtx CompletionContext) (*CompletionValues, *RPCError) {
	if ref.Type != "ref/tool" || ref.Name != "fs_read" || arg.Name != "clientId" {
		return &CompletionValues{Values: []string{}}, nil
	}
	total := 1
	return &CompletionValues{Values: []string{"dev-" + arg.Value}, Total: &total}, nil
}

func TestHandler_CompletionRouting(t *testing.T) {
	store := session.NewMemoryStore(10)
	h := NewHandler(store, nil)
	h.Completions = stubCompletions{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	sid := doInitialize(t, srv)

	post := func(body string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set(SessionIDHeader, sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	out := post(`{"jsonrpc":"2.0","id":1,"method":"completion/complete","params":{"ref":{"type":"ref/tool","name":"fs_read"},"argument":{"name":"clientId","value":"1"}}}`)
	completion := out["result"].(map[string]any)["completion"].(map[string]any)
	values := completion["values"].([]any)
	if len(values) != 1 || values[0] != "dev-1" {
		t.Fatalf("values = %+v", values)
	}

	if out = post(`{"jsonrpc":"2.0","id":2,"method":"completion/complete","params":{}}`); out["error"] == nil {
		t.Fatal("missing ref/argument must error")
	}
}
