package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/audit"
	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

func newSysinfoTestDeps(t *testing.T, dispatch *fakeDispatcher) SysinfoDeps {
	t.Helper()
	auditLogger, err := audit.NewLogger(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { auditLogger.Close() })
	return SysinfoDeps{Bridge: dispatch, Audit: auditLogger}
}

func callSysinfoGet(deps SysinfoDeps, sess *session.Session, input map[string]any) (*transport.ToolCallResult, *transport.RPCError) {
	args, _ := json.Marshal(input)
	return deps.handle(context.Background(), sess, transport.ToolCallMeta{RequestID: "req-1"}, args)
}

func TestSysinfoGet_MissingClientID(t *testing.T) {
	deps := newSysinfoTestDeps(t, &fakeDispatcher{})
	sess := session.New(context.Background(), "sess-1", 10)

	_, rpcErr := callSysinfoGet(deps, sess, map[string]any{})
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("rpcErr = %+v, want code -32602", rpcErr)
	}
}

func TestSysinfoGet_SectionsSubset_OnlyCPUPopulated(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{
			Tool:   "sysinfo_get",
			Output: map[string]any{"cpu": map[string]any{"model": "Test CPU", "cores": 4}},
		}, nil
	}}
	deps := newSysinfoTestDeps(t, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callSysinfoGet(deps, sess, map[string]any{"clientId": "dev-1", "sections": []string{"cpu"}})
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
	if out["cpu"] == nil {
		t.Fatal("expected cpu section to be populated")
	}
	if out["memory"] != nil {
		t.Fatalf("expected memory section to be nil/omitted, got %v", out["memory"])
	}
	if out["clientId"] != "dev-1" {
		t.Fatalf("clientId = %v, want dev-1", out["clientId"])
	}
}

func TestSysinfoGet_PartialAgentFailure_NullSectionNotWholeError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{
			Tool: "sysinfo_get",
			Output: map[string]any{
				"cpu":    nil,
				"memory": map[string]any{"totalKB": 1000},
			},
		}, nil
	}}
	deps := newSysinfoTestDeps(t, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callSysinfoGet(deps, sess, map[string]any{"clientId": "dev-1"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("a null section should not fail the whole call: %+v", result.Content)
	}
	var out map[string]any
	if err := json.Unmarshal(result.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["cpu"] != nil {
		t.Fatalf("expected cpu = nil, got %v", out["cpu"])
	}
	if out["memory"] == nil {
		t.Fatal("expected memory section to still be populated")
	}
}

func TestSysinfoGet_OfflineDevice_ToolError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrDeviceOffline
	}}
	deps := newSysinfoTestDeps(t, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callSysinfoGet(deps, sess, map[string]any{"clientId": "dev-1"})
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
