package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/audit"
	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

func newProcessTestDeps(t *testing.T, skipConfirm bool, dispatch *fakeDispatcher) ProcessDeps {
	t.Helper()
	auditLogger, err := audit.NewLogger(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { auditLogger.Close() })
	return ProcessDeps{Bridge: dispatch, Audit: auditLogger, SkipConfirm: skipConfirm, ElicitationTimeout: 2 * time.Second}
}

func TestProcessList_FilterSortLimit_PassedThrough(t *testing.T) {
	var gotInput map[string]any
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		raw, _ := json.Marshal(input)
		_ = json.Unmarshal(raw, &gotInput)
		return protocol.ResultPayload{
			Tool:   "process_list",
			Output: map[string]any{"processes": []any{}, "totalCount": 0},
		}, nil
	}}
	deps := newProcessTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "filter": "chrome", "user": "alice", "sortBy": "cpu", "limit": 5})
	result, rpcErr := deps.handleList(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true: %+v", result.Content)
	}
	if gotInput["filter"] != "chrome" || gotInput["sortBy"] != "cpu" {
		t.Fatalf("input not passed through: %+v", gotInput)
	}
}

func TestProcessInfo_UnknownPID_ToolError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{Tool: "process_info", IsError: true, Error: "process not found"}, nil
	}}
	deps := newProcessTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "pid": 999999})
	result, rpcErr := deps.handleInfo(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("expected isError=true for unknown pid")
	}
}

func TestProcessSignal_RequiresConfirmation_DeclinedNoDispatch(t *testing.T) {
	dispatchCalled := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatchCalled = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newProcessTestDeps(t, false, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "pid": 1234, "signal": "SIGKILL"})
		result, _ := deps.handleSignal(context.Background(), sess, transport.ToolCallMeta{}, args)
		resultCh <- result
	}()

	deliverElicitationResponse(t, sess, map[string]any{"action": "decline"})

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("declined result should not be isError: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for declined result")
	}
	if dispatchCalled {
		t.Fatal("agent should never have received a dispatch after decline")
	}
}

func TestProcessSignal_SelfSignalRejection_SurfacedAsToolError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{Tool: "process_signal", IsError: true, Error: "executor: refusing to signal the agent's own process"}, nil
	}}
	deps := newProcessTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "pid": 1, "signal": "SIGTERM"})
	result, rpcErr := deps.handleSignal(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("expected isError=true for self-signal rejection")
	}
	if !strings.Contains(result.Content[0].Text, "refusing") {
		t.Fatalf("error text = %q, want to mention refusing", result.Content[0].Text)
	}
}

func TestProcessSignal_SkipConfirm_DispatchesDirectly(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{
			Tool:   "process_signal",
			Output: map[string]any{"signalSent": true, "pid": 1234, "signal": "SIGTERM"},
		}, nil
	}}
	deps := newProcessTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "pid": 1234})
	result, rpcErr := deps.handleSignal(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true: %+v", result.Content)
	}
}
