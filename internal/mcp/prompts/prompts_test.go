package prompts

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/devices"
	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

type fakeDispatcher struct {
	fn func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error)
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
	if f.fn == nil {
		return protocol.ResultPayload{}, agent.ErrDeviceOffline
	}
	return f.fn(ctx, deviceID, correlationID, tool, sessionID, input, onProgress)
}

func newTestRegistry(t *testing.T, dispatch *fakeDispatcher) (*Registry, *devices.FileRegistry) {
	t.Helper()
	deviceReg, err := devices.NewFileRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	if dispatch == nil {
		dispatch = &fakeDispatcher{}
	}
	return NewRegistry(deviceReg, dispatch), deviceReg
}

func pairDevice(t *testing.T, reg *devices.FileRegistry, online bool) string {
	t.Helper()
	ctx := context.Background()
	pc, err := reg.CreatePairingCode(ctx, "host-1")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, _, err := reg.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	if err := reg.SetOnline(ctx, device.ID, online); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	return device.ID
}

func get(t *testing.T, r *Registry, name string, args map[string]string) (*transport.PromptResult, *transport.RPCError) {
	t.Helper()
	sess := session.New(context.Background(), "sess-1", 10)
	return r.GetPrompt(context.Background(), sess, name, args)
}

func promptText(t *testing.T, res *transport.PromptResult) string {
	t.Helper()
	if len(res.Messages) != 1 || res.Messages[0].Role != "user" || res.Messages[0].Content.Type != "text" {
		t.Fatalf("messages = %+v", res.Messages)
	}
	return res.Messages[0].Content.Text
}

func TestListPrompts_AllThreeWithArguments(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	list := r.ListPrompts(context.Background())
	if len(list) != 3 {
		t.Fatalf("got %d prompts, want 3", len(list))
	}
	byName := map[string]transport.PromptDescriptor{}
	for _, p := range list {
		byName[p.Name] = p
	}
	requiredArgs := map[string][]string{
		"diagnose_system": {"clientId", "symptom"},
		"safe_cleanup":    {"clientId"},
		"shell_workflow":  {"clientId", "task"},
	}
	for name, wantReq := range requiredArgs {
		p, ok := byName[name]
		if !ok {
			t.Errorf("missing prompt %s", name)
			continue
		}
		var req []string
		for _, a := range p.Arguments {
			if a.Required {
				req = append(req, a.Name)
			}
		}
		if strings.Join(req, ",") != strings.Join(wantReq, ",") {
			t.Errorf("%s required args = %v, want %v", name, req, wantReq)
		}
	}
	if len(byName["safe_cleanup"].Arguments) != 3 {
		t.Errorf("safe_cleanup should declare target and minSizeMB as optional args")
	}
}

func TestGetPrompt_UnknownDeviceAndPrompt(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	if _, rpcErr := get(t, r, "diagnose_system", map[string]string{"clientId": "ghost", "symptom": "high cpu"}); rpcErr == nil {
		t.Fatal("unknown device must error")
	}
	if _, rpcErr := get(t, r, "diagnose_system", map[string]string{"symptom": "high cpu"}); rpcErr == nil {
		t.Fatal("missing clientId must error")
	}
	reg2, devices2 := newTestRegistry(t, nil)
	id := pairDevice(t, devices2, true)
	if _, rpcErr := get(t, reg2, "no_such_prompt", map[string]string{"clientId": id}); rpcErr == nil {
		t.Fatal("unknown prompt must error")
	}
}

func TestDiagnoseSystem_EmbedsSnapshotAndSymptom(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		if tool != "sysinfo_get" {
			t.Errorf("tool = %q", tool)
		}
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{"hostname": "box-42", "cpu": map[string]any{"loadAvg1": 7.5}}}, nil
	}}
	r, deviceReg := newTestRegistry(t, dispatch)
	id := pairDevice(t, deviceReg, true)

	res, rpcErr := get(t, r, "diagnose_system", map[string]string{"clientId": id, "symptom": "high cpu"})
	if rpcErr != nil {
		t.Fatalf("rpcErr = %+v", rpcErr)
	}
	text := promptText(t, res)
	for _, want := range []string{"box-42", "high cpu", id, "sortBy=\"cpu\"", "process_list"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestDiagnoseSystem_MemorySymptomPicksMemoryMetric(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{}}, nil
	}}
	r, deviceReg := newTestRegistry(t, dispatch)
	id := pairDevice(t, deviceReg, true)

	res, rpcErr := get(t, r, "diagnose_system", map[string]string{"clientId": id, "symptom": "Memory pressure"})
	if rpcErr != nil {
		t.Fatalf("rpcErr = %+v", rpcErr)
	}
	if !strings.Contains(promptText(t, res), "sortBy=\"memory\"") {
		t.Fatal("memory symptom should sort process_list by memory")
	}
}

func TestDiagnoseSystem_OfflineDeviceErrors(t *testing.T) {
	r, deviceReg := newTestRegistry(t, nil)
	id := pairDevice(t, deviceReg, false)
	_, rpcErr := get(t, r, "diagnose_system", map[string]string{"clientId": id, "symptom": "disk full"})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "offline") {
		t.Fatalf("rpcErr = %+v, want offline error", rpcErr)
	}
}

func TestDiagnoseSystem_SnapshotFailureStillRenders(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrConnectionClosed
	}}
	r, deviceReg := newTestRegistry(t, dispatch)
	id := pairDevice(t, deviceReg, true)

	res, rpcErr := get(t, r, "diagnose_system", map[string]string{"clientId": id, "symptom": "high cpu"})
	if rpcErr != nil {
		t.Fatalf("rpcErr = %+v", rpcErr)
	}
	if !strings.Contains(promptText(t, res), "snapshot unavailable") {
		t.Fatal("failed snapshot should degrade to a placeholder, not an error")
	}
}

func TestSafeCleanup_RespectsTargetAndMinSize(t *testing.T) {
	r, deviceReg := newTestRegistry(t, nil)
	id := pairDevice(t, deviceReg, true)

	res, rpcErr := get(t, r, "safe_cleanup", map[string]string{"clientId": id, "target": "disk", "minSizeMB": "250"})
	if rpcErr != nil {
		t.Fatalf("rpcErr = %+v", rpcErr)
	}
	text := promptText(t, res)
	if !strings.Contains(text, "250 MB") || !strings.Contains(text, "fs_list") {
		t.Fatalf("prompt missing disk scope/size: %s", text)
	}
	if strings.Contains(text, "orphaned") {
		t.Fatal("target=disk must not include the processes step")
	}

	res, _ = get(t, r, "safe_cleanup", map[string]string{"clientId": id})
	text = promptText(t, res)
	if !strings.Contains(text, "100 MB") || !strings.Contains(text, "orphaned") || !strings.Contains(text, "fs_list") {
		t.Fatal("defaults (all, 100MB) should include both scopes")
	}

	if _, rpcErr := get(t, r, "safe_cleanup", map[string]string{"clientId": id, "target": "everything"}); rpcErr == nil {
		t.Fatal("invalid target must error")
	}
	if _, rpcErr := get(t, r, "safe_cleanup", map[string]string{"clientId": id, "minSizeMB": "-3"}); rpcErr == nil {
		t.Fatal("invalid minSizeMB must error")
	}
}

func TestShellWorkflow_EmbedsTaskAndClient(t *testing.T) {
	r, deviceReg := newTestRegistry(t, nil)
	id := pairDevice(t, deviceReg, true)

	res, rpcErr := get(t, r, "shell_workflow", map[string]string{"clientId": id, "task": "rotate the nginx logs"})
	if rpcErr != nil {
		t.Fatalf("rpcErr = %+v", rpcErr)
	}
	text := promptText(t, res)
	for _, want := range []string{"rotate the nginx logs", id, "shell_session_start", "shell_session_write", "shell_session_close"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	if _, rpcErr := get(t, r, "shell_workflow", map[string]string{"clientId": id}); rpcErr == nil {
		t.Fatal("missing task must error")
	}
}
