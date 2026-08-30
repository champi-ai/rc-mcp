package tools

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/CloudKeter/rc-mcp/internal/agent"
	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/devices"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/session"
	"github.com/CloudKeter/rc-mcp/internal/shellpolicy"
	"github.com/CloudKeter/rc-mcp/internal/transport"
)

// fakeDispatcher is a fast, in-process stand-in for *agent.Bridge, used by
// most shell_exec tests so they don't need a live WebSocket-connected
// agent. One end-to-end test (below) exercises the real Bridge/Hub/agent
// wire path instead.
type fakeDispatcher struct {
	fn func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error)
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
	return f.fn(ctx, deviceID, correlationID, tool, sessionID, input, onProgress)
}

func newTestDeps(t *testing.T, skipConfirm bool, dispatch *fakeDispatcher) ShellExecDeps {
	t.Helper()
	auditLogger, err := audit.NewLogger(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { auditLogger.Close() })

	return ShellExecDeps{
		Bridge:             dispatch,
		Audit:              auditLogger,
		SkipConfirm:        skipConfirm,
		ElicitationTimeout: 2 * time.Second,
	}
}

func callShellExec(deps ShellExecDeps, sess *session.Session, input map[string]any) (*transport.ToolCallResult, *transport.RPCError) {
	args, _ := json.Marshal(input)
	return deps.handle(context.Background(), sess, transport.ToolCallMeta{RequestID: "req-1"}, args)
}

func TestShellExec_InvalidParams_MissingCommand(t *testing.T) {
	deps := newTestDeps(t, true, &fakeDispatcher{})
	sess := session.New(context.Background(), "sess-1", 10)

	_, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": "dev-1"})
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("rpcErr = %+v, want code -32602", rpcErr)
	}
}

func TestShellExec_InvalidParams_TimeoutOutOfRange(t *testing.T) {
	deps := newTestDeps(t, true, &fakeDispatcher{})
	sess := session.New(context.Background(), "sess-1", 10)

	_, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "echo hi", "timeout": 9999})
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("rpcErr = %+v, want code -32602", rpcErr)
	}
}

func TestShellExec_RoundTrip_SkipConfirm(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{
			Tool:   "shell_exec",
			Output: map[string]any{"stdout": "hello\n", "stderr": "", "exitCode": 0, "killed": false, "durationMs": 5},
		}, nil
	}}
	deps := newTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "echo hello"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, content = %+v", result.Content)
	}

	var out map[string]any
	if err := json.Unmarshal(result.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	if out["stdout"] != "hello\n" {
		t.Fatalf("stdout = %v, want %q", out["stdout"], "hello\n")
	}
	if out["exitCode"] != float64(0) {
		t.Fatalf("exitCode = %v, want 0", out["exitCode"])
	}
	if out["killed"] != false {
		t.Fatalf("killed = %v, want false", out["killed"])
	}
	if out["clientId"] != "dev-1" {
		t.Fatalf("clientId = %v, want dev-1", out["clientId"])
	}
}

func TestShellExec_OfflineDevice_ToolError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrDeviceOffline
	}}
	deps := newTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": "not-a-real-device", "command": "echo hi"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result == nil || !result.IsError {
		t.Fatalf("result = %+v, want IsError=true", result)
	}
	if !strings.Contains(result.Content[0].Text, "offline") {
		t.Fatalf("error text = %q, want to mention offline", result.Content[0].Text)
	}
}

func TestShellExec_AgentDisconnect_ToolError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrConnectionClosed
	}}
	deps := newTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "echo hi"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result == nil || !result.IsError {
		t.Fatalf("result = %+v, want IsError=true", result)
	}
	if !strings.Contains(result.Content[0].Text, "disconnected") {
		t.Fatalf("error text = %q, want to mention disconnected", result.Content[0].Text)
	}
}

func TestShellExec_Timeout_KilledWithPartialOutput(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{
			Tool:   "shell_exec",
			Output: map[string]any{"stdout": "partial\n", "stderr": "", "exitCode": -1, "killed": true, "durationMs": 2000},
		}, nil
	}}
	deps := newTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "sleep 10", "timeout": 2})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, want false (a timeout is not a tool error)")
	}
	var out map[string]any
	if err := json.Unmarshal(result.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["killed"] != true {
		t.Fatalf("killed = %v, want true", out["killed"])
	}
	if out["stdout"] != "partial\n" {
		t.Fatalf("stdout = %v, want partial output preserved", out["stdout"])
	}
}

func TestShellExec_ElicitationDeclined_NoDispatchSent(t *testing.T) {
	dispatchCalled := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatchCalled = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newTestDeps(t, false, dispatch) // confirmation required
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		result, _ := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "rm -rf /tmp/whatever"})
		resultCh <- result
	}()

	id := deliverElicitationResponse(t, sess, map[string]any{"action": "decline"})
	_ = id

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("declined result should not be isError: %+v", result)
		}
		var out map[string]any
		if err := json.Unmarshal(result.StructuredContent, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out["declined"] != true {
			t.Fatalf("out = %v, want declined=true", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for declined result")
	}

	if dispatchCalled {
		t.Fatal("agent should never have received a dispatch after decline")
	}
}

func TestShellExec_ElicitationAccepted_DispatchProceeds(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{
			Tool:   "shell_exec",
			Output: map[string]any{"stdout": "ok\n", "stderr": "", "exitCode": 0, "killed": false, "durationMs": 1},
		}, nil
	}}
	deps := newTestDeps(t, false, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		result, _ := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "echo ok"})
		resultCh <- result
	}()

	deliverElicitationResponse(t, sess, map[string]any{"action": "accept", "content": map[string]any{"confirm": true}})

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("result.IsError = true: %+v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result after accepted elicitation")
	}
}

func TestShellExec_ProgressStreamedBeforeResult(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		if onProgress != nil {
			onProgress(&protocol.ProgressPayload{Tool: "shell_exec", Message: "partial output chunk"}, nil)
		}
		return protocol.ResultPayload{
			Tool:   "shell_exec",
			Output: map[string]any{"stdout": "done\n", "stderr": "", "exitCode": 0, "killed": false, "durationMs": 1},
		}, nil
	}}
	deps := newTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		result, _ := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "sleep 5 && echo done"})
		resultCh <- result
	}()

	select {
	case ev := <-sess.EventCh:
		if !strings.Contains(ev.Data, "notifications/progress") {
			t.Fatalf("event = %s, want a notifications/progress message", ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for progress notification")
	}

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("result.IsError = true: %+v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for final result")
	}
}

// deliverElicitationResponse waits for the next elicitation/create SSE
// event on sess and delivers resultPayload as the client's response,
// returning the elicitation's request ID.
func deliverElicitationResponse(t *testing.T, sess *session.Session, resultPayload map[string]any) string {
	t.Helper()
	var ev session.SSEEvent
	select {
	case ev = <-sess.EventCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for elicitation/create")
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
		t.Fatalf("unmarshal elicitation event: %v", err)
	}
	id, _ := msg["id"].(string)
	if id == "" {
		t.Fatalf("elicitation event missing id: %s", ev.Data)
	}
	resp, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  resultPayload,
	})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if !sess.DeliverResponse(id, resp) {
		t.Fatal("DeliverResponse: no waiting handler found")
	}
	return id
}

// --- End-to-end test against the real Bridge/Hub/agent wire path ---

func TestShellExec_EndToEnd_RealAgent(t *testing.T) {
	devReg, err := devices.NewFileRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	hub := agent.NewHub(devReg, time.Minute)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	// Wait for the agent connection's goroutine (and its registry writes)
	// to finish before the TempDir cleanup runs; deferred in registration
	// order, so this runs before srv.Close() above.
	defer hub.Shutdown("test_end")

	ctx := context.Background()
	pc, err := devReg.CreatePairingCode(ctx, "test-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, token, err := devReg.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	if err := devReg.UpdateCapabilities(ctx, device.ID, []string{"shell"}); err != nil {
		t.Fatalf("UpdateCapabilities: %v", err)
	}

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(protocol.Envelope{
		Type:            protocol.MsgHello,
		ID:              "hello-1",
		ProtocolVersion: protocol.Version,
		Ts:              time.Now().UTC(),
		Payload: protocol.HelloPayload{
			DeviceToken:  token,
			Hostname:     "test-host",
			Capabilities: []string{"shell"},
		},
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	var ack protocol.Envelope
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	if ack.Type != protocol.MsgHelloAck {
		t.Fatalf("expected hello_ack, got %s", ack.Type)
	}

	deadline := time.Now().Add(3 * time.Second)
	for hub.AgentsOnline() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.AgentsOnline() != 1 {
		t.Fatal("agent never came online")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil || env.Type != protocol.MsgDispatch {
			return
		}
		_ = conn.WriteJSON(protocol.Envelope{
			Type: protocol.MsgResult,
			ID:   env.ID,
			Ts:   time.Now().UTC(),
			Payload: protocol.ResultPayload{
				Tool:   "shell_exec",
				Output: map[string]any{"stdout": "hello\n", "stderr": "", "exitCode": 0, "killed": false, "durationMs": 5},
			},
		})
	}()
	t.Cleanup(wg.Wait)

	auditLogger, err := audit.NewLogger(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer auditLogger.Close()

	deps := ShellExecDeps{
		Bridge:             agent.NewBridge(hub),
		Audit:              auditLogger,
		SkipConfirm:        true,
		ElicitationTimeout: 2 * time.Second,
	}
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": device.ID, "command": "echo hello"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true: %+v", result.Content)
	}
	var out map[string]any
	if err := json.Unmarshal(result.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["stdout"] != "hello\n" || out["exitCode"] != float64(0) || out["killed"] != false {
		t.Fatalf("out = %+v, want stdout=hello exitCode=0 killed=false", out)
	}
}

func TestShellExec_DenylistBlocksBeforeDispatch(t *testing.T) {
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newTestDeps(t, true, dispatch)
	pol, err := shellpolicy.New([]string{`rm\s+-rf`}, nil)
	if err != nil {
		t.Fatalf("shellpolicy.New: %v", err)
	}
	deps.Policy = pol
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "rm -rf /tmp/x"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatalf("blocked command should be a tool error: %+v", result)
	}
	if dispatched {
		t.Fatal("a denylisted command must never reach the dispatcher")
	}
}

func TestShellExec_AllowlistPermitsMatchingCommand(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{"stdout": "ok\n", "stderr": "", "exitCode": 0, "killed": false, "durationMs": 1}}, nil
	}}
	deps := newTestDeps(t, true, dispatch)
	pol, err := shellpolicy.New(nil, []string{`^ls\b`})
	if err != nil {
		t.Fatalf("shellpolicy.New: %v", err)
	}
	deps.Policy = pol
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "ls -la"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("allowlisted command should succeed: %+v", result.Content)
	}
}

func TestShellExec_AllowlistBlocksNonMatchingCommand(t *testing.T) {
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newTestDeps(t, true, dispatch)
	pol, err := shellpolicy.New(nil, []string{`^ls\b`})
	if err != nil {
		t.Fatalf("shellpolicy.New: %v", err)
	}
	deps.Policy = pol
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callShellExec(deps, sess, map[string]any{"clientId": "dev-1", "command": "cat /etc/passwd"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("a command not on the allowlist must be blocked")
	}
	if dispatched {
		t.Fatal("a blocked command must never reach the dispatcher")
	}
}
