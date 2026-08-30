package tools

// Cross-tool error taxonomy suite (Section 13, issue #34): each error
// category is exercised once against a representative tool from every
// tool group, asserting the exact response shape so the same failure
// looks the same regardless of which tool triggered it.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/jobs"
	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

// taxonomyRegistry builds a registry with one representative tool per
// group, all backed by dispatch.
func taxonomyRegistry(t *testing.T, dispatch *fakeDispatcher) (*Registry, string) {
	t.Helper()
	reg, deviceReg := newTestRegistry(t)
	clientID := pairOnlineDevice(t, deviceReg, []string{"shell", "fs", "process", "sysinfo", "screenshot"})

	reg.Register(NewShellExecDefinition(ShellExecDeps{Bridge: dispatch, SkipConfirm: true}))
	reg.Register(NewFSReadDefinition(FSDeps{Bridge: dispatch, SkipConfirm: true}))
	reg.Register(NewProcessListDefinition(ProcessDeps{Bridge: dispatch, SkipConfirm: true}))
	reg.Register(NewSysinfoGetDefinition(SysinfoDeps{Bridge: dispatch}))
	reg.Register(NewScreenshotCaptureDefinition(ScreenshotDeps{
		Bridge: dispatch,
		Jobs:   jobs.NewMemoryStore(0, nil),
		Online: func(string) bool { return true },
	}))
	return reg, clientID
}

// representative args per tool, minus clientId (added per test).
func taxonomyArgs(tool, clientID string) json.RawMessage {
	base := map[string]any{"clientId": clientID}
	switch tool {
	case "shell_exec":
		base["command"] = "echo hi"
	case "fs_read":
		base["path"] = "/etc/hostname"
	}
	raw, _ := json.Marshal(base)
	return raw
}

var taxonomyTools = []string{"shell_exec", "fs_read", "process_list", "sysinfo_get", "screenshot_capture"}

func callViaRegistry(t *testing.T, reg *Registry, tool string, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	t.Helper()
	sess := session.New(context.Background(), "sess-tax", 10)
	return reg.CallTool(context.Background(), sess, transport.ToolCallMeta{RequestID: "req-tax"}, tool, args)
}

func TestTaxonomy_MethodNotFound(t *testing.T) {
	reg, _ := taxonomyRegistry(t, &fakeDispatcher{})
	_, rpcErr := callViaRegistry(t, reg, "no_such_tool", json.RawMessage(`{}`))
	if rpcErr == nil || rpcErr.Code != -32601 {
		t.Fatalf("rpcErr = %+v, want -32601", rpcErr)
	}
}

func TestTaxonomy_InvalidParams_SharedValidationPath(t *testing.T) {
	// The handler must never run: validation happens centrally in
	// CallTool, and the -32602 error carries structured validationErrors.
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	reg, clientID := taxonomyRegistry(t, dispatch)

	cases := map[string]json.RawMessage{
		"shell_exec":         json.RawMessage(`{"command":"echo hi"}`),                                          // missing clientId
		"fs_read":            json.RawMessage(fmt.Sprintf(`{"clientId":%q,"path":"/x","offset":-1}`, clientID)), // below minimum
		"process_list":       json.RawMessage(fmt.Sprintf(`{"clientId":%q,"sortBy":"badfield"}`, clientID)),     // enum violation
		"sysinfo_get":        json.RawMessage(fmt.Sprintf(`{"clientId":%q,"sections":"cpu"}`, clientID)),        // wrong type
		"screenshot_capture": json.RawMessage(fmt.Sprintf(`{"clientId":%q,"quality":99}`, clientID)),            // above maximum
	}
	for tool, args := range cases {
		_, rpcErr := callViaRegistry(t, reg, tool, args)
		if rpcErr == nil || rpcErr.Code != -32602 {
			t.Errorf("%s: rpcErr = %+v, want -32602", tool, rpcErr)
			continue
		}
		data, ok := rpcErr.Data.(map[string]any)
		if !ok || data["validationErrors"] == nil {
			t.Errorf("%s: Data = %+v, want validationErrors", tool, rpcErr.Data)
		}
	}
	if dispatched {
		t.Fatal("a handler dispatched despite failing validation")
	}
}

func TestTaxonomy_UnknownDevice(t *testing.T) {
	reg, _ := taxonomyRegistry(t, &fakeDispatcher{})
	for _, tool := range taxonomyTools {
		result, rpcErr := callViaRegistry(t, reg, tool, taxonomyArgs(tool, "ghost-device"))
		if rpcErr != nil {
			t.Errorf("%s: unexpected rpcErr %+v", tool, rpcErr)
			continue
		}
		assertToolErrorText(t, tool, result, "Unknown device ghost-device")
	}
}

func TestTaxonomy_DeviceOffline(t *testing.T) {
	reg, deviceReg := newTestRegistry(t)
	clientID := pairOnlineDevice(t, deviceReg, []string{"shell", "fs", "process", "sysinfo", "screenshot"})
	if err := deviceReg.SetOnline(context.Background(), clientID, false); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	dispatch := &fakeDispatcher{}
	reg.Register(NewShellExecDefinition(ShellExecDeps{Bridge: dispatch, SkipConfirm: true}))
	reg.Register(NewFSReadDefinition(FSDeps{Bridge: dispatch, SkipConfirm: true}))
	reg.Register(NewProcessListDefinition(ProcessDeps{Bridge: dispatch, SkipConfirm: true}))
	reg.Register(NewSysinfoGetDefinition(SysinfoDeps{Bridge: dispatch}))
	reg.Register(NewScreenshotCaptureDefinition(ScreenshotDeps{Bridge: dispatch, Jobs: jobs.NewMemoryStore(0, nil)}))

	for _, tool := range taxonomyTools {
		result, rpcErr := callViaRegistry(t, reg, tool, taxonomyArgs(tool, clientID))
		if rpcErr != nil {
			t.Errorf("%s: unexpected rpcErr %+v", tool, rpcErr)
			continue
		}
		assertToolErrorText(t, tool, result, fmt.Sprintf("Device %s is offline", clientID))
	}
}

func TestTaxonomy_CapabilityDisabled(t *testing.T) {
	reg, deviceReg := newTestRegistry(t)
	clientID := pairOnlineDevice(t, deviceReg, []string{}) // online, no capabilities
	dispatch := &fakeDispatcher{}
	reg.Register(NewShellExecDefinition(ShellExecDeps{Bridge: dispatch, SkipConfirm: true}))
	reg.Register(NewFSReadDefinition(FSDeps{Bridge: dispatch, SkipConfirm: true}))
	reg.Register(NewProcessListDefinition(ProcessDeps{Bridge: dispatch, SkipConfirm: true}))
	reg.Register(NewSysinfoGetDefinition(SysinfoDeps{Bridge: dispatch}))
	reg.Register(NewScreenshotCaptureDefinition(ScreenshotDeps{Bridge: dispatch, Jobs: jobs.NewMemoryStore(0, nil)}))

	wantCap := map[string]string{
		"shell_exec":         "shell",
		"fs_read":            "fs",
		"process_list":       "process",
		"sysinfo_get":        "sysinfo",
		"screenshot_capture": "screenshot",
	}
	for _, tool := range taxonomyTools {
		result, rpcErr := callViaRegistry(t, reg, tool, taxonomyArgs(tool, clientID))
		if rpcErr != nil {
			t.Errorf("%s: unexpected rpcErr %+v", tool, rpcErr)
			continue
		}
		assertToolErrorText(t, tool, result, fmt.Sprintf("Device %s does not have %s enabled", clientID, wantCap[tool]))
	}
}

func TestTaxonomy_AgentDisconnectMidOp(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrConnectionClosed
	}}
	reg, clientID := taxonomyRegistry(t, dispatch)

	for _, tool := range taxonomyTools {
		result, rpcErr := callViaRegistry(t, reg, tool, taxonomyArgs(tool, clientID))
		if rpcErr != nil {
			t.Errorf("%s: unexpected rpcErr %+v", tool, rpcErr)
			continue
		}
		assertToolErrorText(t, tool, result, "Agent disconnected during operation")
	}
}

func TestTaxonomy_AgentExecutionError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{Tool: tool, IsError: true, Error: "permission denied"}, nil
	}}
	reg, clientID := taxonomyRegistry(t, dispatch)

	for _, tool := range taxonomyTools {
		result, rpcErr := callViaRegistry(t, reg, tool, taxonomyArgs(tool, clientID))
		if rpcErr != nil {
			t.Errorf("%s: unexpected rpcErr %+v", tool, rpcErr)
			continue
		}
		assertToolErrorText(t, tool, result, "permission denied")
	}
}

func TestTaxonomy_JobFailure_IsJobStatusNotInlineError(t *testing.T) {
	// screenshot_watch (pattern (a)): a failing dispatch does not make
	// the tools/call itself an error -- the jobId is returned and the
	// failure lands in the job store (Section 13, "Job execution
	// failure").
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrConnectionClosed
	}}
	store := jobs.NewMemoryStore(0, nil)
	reg, deviceReg := newTestRegistry(t)
	clientID := pairOnlineDevice(t, deviceReg, []string{"screenshot"})
	reg.Register(NewScreenshotWatchDefinition(ScreenshotDeps{
		Bridge: dispatch, Jobs: store, Cancels: NewWatchCancels(),
		Online: func(string) bool { return true },
	}))

	result, rpcErr := callViaRegistry(t, reg, "screenshot_watch", taxonomyArgs("screenshot_watch", clientID))
	if rpcErr != nil || result.IsError {
		t.Fatalf("watch ack should succeed even if the dispatch fails: rpcErr=%+v result=%+v", rpcErr, result)
	}
	var ack struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(result.StructuredContent, &ack)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job, err := store.Get(ack.JobID); err == nil && job.Status == jobs.JobStatusFailed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job never reached failed status")
}

func TestTaxonomy_ElicitationDeclined_SpecText(t *testing.T) {
	if got := declinedResult("declined").Content[0].Text; got != "Operation declined by user" {
		t.Fatalf("declined text = %q", got)
	}
	if declinedResult("declined").IsError {
		t.Fatal("declined result must not be isError")
	}
	if got := declinedResult("elicitation_timeout").Content[0].Text; got != "Confirmation timed out" {
		t.Fatalf("timeout text = %q", got)
	}
}

func assertToolErrorText(t *testing.T, tool string, result *transport.ToolCallResult, want string) {
	t.Helper()
	if result == nil {
		t.Errorf("%s: nil result", tool)
		return
	}
	if !result.IsError {
		t.Errorf("%s: want isError:true, got %+v", tool, result)
		return
	}
	if len(result.Content) == 0 || result.Content[0].Type != "text" || result.Content[0].Text != want {
		t.Errorf("%s: content = %+v, want text %q", tool, result.Content, want)
	}
}
