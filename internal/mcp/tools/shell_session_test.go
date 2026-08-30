package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/agent"
	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/session"
	"github.com/CloudKeter/rc-mcp/internal/shellpolicy"
	"github.com/CloudKeter/rc-mcp/internal/transport"
)

func newShellSessionTestDeps(t *testing.T, skipConfirm bool, dispatch *fakeDispatcher) ShellSessionDeps {
	t.Helper()
	auditLogger, err := audit.NewLogger(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { auditLogger.Close() })
	return ShellSessionDeps{Bridge: dispatch, Audit: auditLogger, SkipConfirm: skipConfirm, ElicitationTimeout: 2 * time.Second}
}

func TestShellSessionStart_RoundTrip_RecordsMapping(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{
			Tool:   "shell_session_start",
			Output: map[string]any{"shellSessionId": "shsess_1", "pid": 4242, "shell": "/bin/bash"},
		}, nil
	}}
	deps := newShellSessionTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1"})
	result, rpcErr := deps.handleStart(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true: %+v", result.Content)
	}

	entry, ok := sess.GetShellSession("shsess_1")
	if !ok {
		t.Fatal("expected shell session mapping to be recorded")
	}
	if entry.ClientID != "dev-1" || entry.PID != 4242 {
		t.Fatalf("entry = %+v, want ClientID=dev-1 PID=4242", entry)
	}
}

func TestShellSessionStart_ExceedsMaxSessions_ToolError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		t.Fatal("dispatch should not be reached once the max session cap is hit")
		return protocol.ResultPayload{}, nil
	}}
	deps := newShellSessionTestDeps(t, true, dispatch)
	deps.MaxSessions = 1
	sess := session.New(context.Background(), "sess-1", 10)
	sess.SetShellSession("existing", &session.ShellSessionEntry{ClientID: "dev-1"})

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1"})
	result, rpcErr := deps.handleStart(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("expected isError=true when max shell sessions exceeded")
	}
}

func TestShellSessionWrite_UnknownSession_ToolError(t *testing.T) {
	deps := newShellSessionTestDeps(t, true, &fakeDispatcher{})
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"shellSessionId": "nope", "input": "echo hi\n"})
	result, rpcErr := deps.handleWrite(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("expected isError=true for unknown shell session")
	}
}

func TestShellSessionWrite_ResolvesClientIDFromMapping(t *testing.T) {
	var gotDeviceID string
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		gotDeviceID = deviceID
		return protocol.ResultPayload{
			Tool:   "shell_session_write",
			Output: map[string]any{"bytesWritten": 11, "output": "hello\n"},
		}, nil
	}}
	deps := newShellSessionTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)
	sess.SetShellSession("shsess_1", &session.ShellSessionEntry{ClientID: "dev-42", PID: 1, Shell: "/bin/bash"})

	args, _ := json.Marshal(map[string]any{"shellSessionId": "shsess_1", "input": "echo hello\n"})
	result, rpcErr := deps.handleWrite(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true: %+v", result.Content)
	}
	if gotDeviceID != "dev-42" {
		t.Fatalf("deviceID = %q, want dev-42 (resolved from mapping, not input)", gotDeviceID)
	}
}

func TestShellSessionWrite_ExitedRemovesMapping(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		exitCode := 0
		return protocol.ResultPayload{
			Tool:   "shell_session_write",
			Output: map[string]any{"bytesWritten": 5, "exited": true, "exitCode": exitCode},
		}, nil
	}}
	deps := newShellSessionTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)
	sess.SetShellSession("shsess_1", &session.ShellSessionEntry{ClientID: "dev-1"})

	args, _ := json.Marshal(map[string]any{"shellSessionId": "shsess_1", "input": "exit\n"})
	_, rpcErr := deps.handleWrite(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if _, ok := sess.GetShellSession("shsess_1"); ok {
		t.Fatal("expected shell session mapping to be removed after exit")
	}
}

func TestShellSessionClose_RemovesMappingEvenOnAgentOffline(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrDeviceOffline
	}}
	deps := newShellSessionTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)
	sess.SetShellSession("shsess_1", &session.ShellSessionEntry{ClientID: "dev-1"})

	args, _ := json.Marshal(map[string]any{"shellSessionId": "shsess_1"})
	result, rpcErr := deps.handleClose(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("expected isError=true when agent is offline")
	}
	if _, ok := sess.GetShellSession("shsess_1"); ok {
		t.Fatal("expected server-side mapping to be cleaned up despite the dispatch error")
	}
}

func TestShellSessionClose_UnknownSession_ToolError(t *testing.T) {
	deps := newShellSessionTestDeps(t, true, &fakeDispatcher{})
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"shellSessionId": "nope"})
	result, rpcErr := deps.handleClose(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("expected isError=true for unknown shell session")
	}
}

func TestShellSessionStart_ElicitationDeclined_NoDispatchSent(t *testing.T) {
	dispatchCalled := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatchCalled = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newShellSessionTestDeps(t, false, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		args, _ := json.Marshal(map[string]any{"clientId": "dev-1"})
		result, _ := deps.handleStart(context.Background(), sess, transport.ToolCallMeta{}, args)
		resultCh <- result
	}()

	deliverElicitationResponse(t, sess, map[string]any{"action": "decline"})

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("declined result should not be isError: %+v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	if dispatchCalled {
		t.Fatal("agent should never have received a dispatch after decline")
	}
}

func TestShellSessionStart_DenylistedShellBlockedBeforeDispatch(t *testing.T) {
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newShellSessionTestDeps(t, true, dispatch)
	pol, err := shellpolicy.New([]string{`^/bin/zsh$`}, nil)
	if err != nil {
		t.Fatalf("shellpolicy.New: %v", err)
	}
	deps.Policy = pol
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "shell": "/bin/zsh"})
	result, rpcErr := deps.handleStart(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("denylisted shell should be a tool error")
	}
	if dispatched {
		t.Fatal("a denylisted shell must never reach the dispatcher")
	}
}

func TestShellSessionWrite_DenylistBlocksInputBeforeDispatch(t *testing.T) {
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newShellSessionTestDeps(t, true, dispatch)
	pol, err := shellpolicy.New([]string{`sudo`}, nil)
	if err != nil {
		t.Fatalf("shellpolicy.New: %v", err)
	}
	deps.Policy = pol
	sess := session.New(context.Background(), "sess-1", 10)
	sess.SetShellSession("shsess_1", &session.ShellSessionEntry{ClientID: "dev-1", PID: 1, Shell: "/bin/bash"})

	args, _ := json.Marshal(map[string]any{"shellSessionId": "shsess_1", "input": "sudo rm -rf /\n"})
	result, rpcErr := deps.handleWrite(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("denylisted input should be a tool error")
	}
	if dispatched {
		t.Fatal("denylisted input must never reach the dispatcher")
	}
}
