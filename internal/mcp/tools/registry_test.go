package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/champi-ai/rc-mcp/internal/devices"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

func newTestRegistry(t *testing.T) (*Registry, *devices.FileRegistry) {
	t.Helper()
	reg, err := devices.NewFileRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	return NewRegistry(reg), reg
}

// pairOnlineDevice pairs a fresh device with the given capabilities and
// marks it online, returning its ID.
func pairOnlineDevice(t *testing.T, reg *devices.FileRegistry, caps []string) string {
	t.Helper()
	ctx := context.Background()
	pc, err := reg.CreatePairingCode(ctx, "test-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, _, err := reg.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	if err := reg.UpdateCapabilities(ctx, device.ID, caps); err != nil {
		t.Fatalf("UpdateCapabilities: %v", err)
	}
	if err := reg.SetOnline(ctx, device.ID, true); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	return device.ID
}

func stubHandler(called *bool) HandlerFunc {
	return func(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
		if called != nil {
			*called = true
		}
		return &transport.ToolCallResult{Content: []transport.ToolContent{{Type: "text", Text: "ok"}}}, nil
	}
}

func TestRegistry_ListTools_CapabilityGated(t *testing.T) {
	reg, devReg := newTestRegistry(t)
	reg.Register(Definition{Name: "shell_exec", RequiredCapability: "shell", Handler: stubHandler(nil)})

	if tools := reg.ListTools(context.Background()); len(tools) != 0 {
		t.Fatalf("ListTools with no online agents = %v, want empty", tools)
	}

	deviceID := pairOnlineDevice(t, devReg, []string{"shell"})
	tools := reg.ListTools(context.Background())
	if len(tools) != 1 || tools[0].Name != "shell_exec" {
		t.Fatalf("ListTools = %v, want [shell_exec]", tools)
	}

	// Disconnect: capability no longer online.
	if err := devReg.SetOnline(context.Background(), deviceID, false); err != nil {
		t.Fatalf("SetOnline(false): %v", err)
	}
	if tools := reg.ListTools(context.Background()); len(tools) != 0 {
		t.Fatalf("ListTools after disconnect = %v, want empty", tools)
	}
}

func TestRegistry_CallTool_UnknownToolMethodNotFound(t *testing.T) {
	reg, _ := newTestRegistry(t)
	_, rpcErr := reg.CallTool(context.Background(), nil, transport.ToolCallMeta{}, "nonexistent", json.RawMessage(`{}`))
	if rpcErr == nil || rpcErr.Code != -32601 {
		t.Fatalf("rpcErr = %+v, want code -32601", rpcErr)
	}
}

func TestRegistry_CallTool_CapabilityDisabledReturnsToolError(t *testing.T) {
	reg, devReg := newTestRegistry(t)
	called := false
	reg.Register(Definition{Name: "shell_exec", RequiredCapability: "shell", Handler: stubHandler(&called)})

	deviceID := pairOnlineDevice(t, devReg, []string{"sysinfo"}) // no "shell" capability

	args, _ := json.Marshal(map[string]any{"clientId": deviceID, "command": "echo hi"})
	result, rpcErr := reg.CallTool(context.Background(), nil, transport.ToolCallMeta{}, "shell_exec", args)
	if rpcErr != nil {
		t.Fatalf("expected tool error, not protocol error: %+v", rpcErr)
	}
	if result == nil || !result.IsError {
		t.Fatalf("result = %+v, want IsError=true", result)
	}
	if called {
		t.Fatal("handler should not have been invoked")
	}
}

func TestRegistry_CallTool_OfflineDeviceReturnsToolError(t *testing.T) {
	reg, devReg := newTestRegistry(t)
	called := false
	reg.Register(Definition{Name: "shell_exec", RequiredCapability: "shell", Handler: stubHandler(&called)})

	deviceID := pairOnlineDevice(t, devReg, []string{"shell"})
	if err := devReg.SetOnline(context.Background(), deviceID, false); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}

	args, _ := json.Marshal(map[string]any{"clientId": deviceID, "command": "echo hi"})
	result, rpcErr := reg.CallTool(context.Background(), nil, transport.ToolCallMeta{}, "shell_exec", args)
	if rpcErr != nil {
		t.Fatalf("expected tool error, got protocol error: %+v", rpcErr)
	}
	if result == nil || !result.IsError {
		t.Fatalf("result = %+v, want IsError=true", result)
	}
	if called {
		t.Fatal("handler should not have been invoked")
	}
}

func TestRegistry_CallTool_UnknownDeviceReturnsToolError(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.Register(Definition{Name: "shell_exec", RequiredCapability: "shell", Handler: stubHandler(nil)})

	args, _ := json.Marshal(map[string]any{"clientId": "does-not-exist", "command": "echo hi"})
	result, rpcErr := reg.CallTool(context.Background(), nil, transport.ToolCallMeta{}, "shell_exec", args)
	if rpcErr != nil {
		t.Fatalf("expected tool error, got protocol error: %+v", rpcErr)
	}
	if result == nil || !result.IsError {
		t.Fatalf("result = %+v, want IsError=true", result)
	}
}

func TestRegistry_CallTool_Success(t *testing.T) {
	reg, devReg := newTestRegistry(t)
	called := false
	reg.Register(Definition{Name: "shell_exec", RequiredCapability: "shell", Handler: stubHandler(&called)})

	deviceID := pairOnlineDevice(t, devReg, []string{"shell"})
	args, _ := json.Marshal(map[string]any{"clientId": deviceID, "command": "echo hi"})

	result, rpcErr := reg.CallTool(context.Background(), nil, transport.ToolCallMeta{}, "shell_exec", args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result == nil || result.IsError {
		t.Fatalf("result = %+v, want success", result)
	}
	if !called {
		t.Fatal("handler should have been invoked")
	}
}

func TestRegistry_ToolWithNoRequiredCapabilityAlwaysListed(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.Register(Definition{Name: "no_cap_tool", Handler: stubHandler(nil)})

	tools := reg.ListTools(context.Background())
	if len(tools) != 1 || tools[0].Name != "no_cap_tool" {
		t.Fatalf("ListTools = %v, want [no_cap_tool] even with no online agents", tools)
	}
}
