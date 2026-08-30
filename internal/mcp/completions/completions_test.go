package completions

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/CloudKeter/rc-mcp/internal/agent"
	"github.com/CloudKeter/rc-mcp/internal/devices"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/session"
	"github.com/CloudKeter/rc-mcp/internal/transport"
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

func pairDevice(t *testing.T, reg *devices.FileRegistry, label string, online bool) string {
	t.Helper()
	ctx := context.Background()
	pc, err := reg.CreatePairingCode(ctx, label)
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

func complete(t *testing.T, r *Registry, ref transport.CompletionRef, arg transport.CompletionArgument, compCtx transport.CompletionContext) *transport.CompletionValues {
	t.Helper()
	sess := session.New(context.Background(), "sess-1", 10)
	result, rpcErr := r.Complete(context.Background(), sess, ref, arg, compCtx)
	if rpcErr != nil {
		t.Fatalf("Complete: %+v", rpcErr)
	}
	return result
}

func TestCompleteClientID_ReturnsKnownDevices(t *testing.T) {
	r, deviceReg := newTestRegistry(t, nil)
	onlineID := pairDevice(t, deviceReg, "online-host", true)
	offlineID := pairDevice(t, deviceReg, "offline-host", false)

	result := complete(t, r, transport.CompletionRef{Type: "ref/tool", Name: "shell_exec"}, transport.CompletionArgument{Name: "clientId"}, transport.CompletionContext{})
	if len(result.Values) != 2 {
		t.Fatalf("values = %v, want both devices (online and offline)", result.Values)
	}
	found := map[string]bool{}
	for _, v := range result.Values {
		found[v] = true
	}
	if !found[onlineID] || !found[offlineID] {
		t.Fatalf("values = %v, missing a device", result.Values)
	}
}

func TestCompleteClientID_FiltersByPrefix(t *testing.T) {
	r, deviceReg := newTestRegistry(t, nil)
	id := pairDevice(t, deviceReg, "prod-web-1", true)
	_ = pairDevice(t, deviceReg, "staging-db", true)

	// Prefix match is against clientId/label; the paired device's label is
	// its hostname ("prod-web-1"), so filtering on that prefix must return
	// exactly that device and never the unrelated one.
	result := complete(t, r, transport.CompletionRef{Type: "ref/tool", Name: "fs_read"}, transport.CompletionArgument{Name: "clientId", Value: "prod"}, transport.CompletionContext{})
	if len(result.Values) != 1 || result.Values[0] != id {
		t.Fatalf("values = %v, want [%s]", result.Values, id)
	}
}

func TestCompleteClientID_UnknownToolReturnsEmpty(t *testing.T) {
	r, deviceReg := newTestRegistry(t, nil)
	pairDevice(t, deviceReg, "host", true)

	result := complete(t, r, transport.CompletionRef{Type: "ref/tool", Name: "no_such_tool"}, transport.CompletionArgument{Name: "clientId"}, transport.CompletionContext{})
	if len(result.Values) != 0 {
		t.Fatalf("values = %v, want empty for a tool with no clientId argument", result.Values)
	}
}

func TestCompletePath_DispatchesFSList(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		if tool != "fs_list" {
			t.Errorf("tool = %q, want fs_list", tool)
		}
		in, _ := input.(map[string]any)
		if in["path"] != "/var/log" {
			t.Errorf("path = %v, want /var/log", in["path"])
		}
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{
			"entries": []any{
				map[string]any{"name": "app.log", "type": "file"},
				map[string]any{"name": "apache", "type": "dir"},
				map[string]any{"name": "syslog", "type": "file"},
			},
		}}, nil
	}}
	r, deviceReg := newTestRegistry(t, dispatch)
	id := pairDevice(t, deviceReg, "host", true)

	result := complete(t, r, transport.CompletionRef{Type: "ref/tool", Name: "fs_read"}, transport.CompletionArgument{Name: "path", Value: "/var/log/ap"}, transport.CompletionContext{Arguments: map[string]string{"clientId": id}})
	want := map[string]bool{"/var/log/app.log": true, "/var/log/apache/": true}
	if len(result.Values) != 2 {
		t.Fatalf("values = %v, want 2 matches for prefix \"ap\"", result.Values)
	}
	for _, v := range result.Values {
		if !want[v] {
			t.Errorf("unexpected value %q", v)
		}
	}
}

func TestCompletePath_OfflineDeviceDegradesToEmpty(t *testing.T) {
	r, deviceReg := newTestRegistry(t, nil)
	id := pairDevice(t, deviceReg, "host", false)

	result := complete(t, r, transport.CompletionRef{Type: "ref/tool", Name: "fs_read"}, transport.CompletionArgument{Name: "path", Value: "/tmp/"}, transport.CompletionContext{Arguments: map[string]string{"clientId": id}})
	if len(result.Values) != 0 {
		t.Fatalf("values = %v, want empty for offline device", result.Values)
	}
}

func TestCompletePath_NoClientIDInContextDegradesToEmpty(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	result := complete(t, r, transport.CompletionRef{Type: "ref/tool", Name: "fs_read"}, transport.CompletionArgument{Name: "path", Value: "/tmp/"}, transport.CompletionContext{})
	if len(result.Values) != 0 {
		t.Fatalf("values = %v, want empty without a clientId in context", result.Values)
	}
}

func TestCompletePath_DispatchErrorDegradesToEmpty(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrConnectionClosed
	}}
	r, deviceReg := newTestRegistry(t, dispatch)
	id := pairDevice(t, deviceReg, "host", true)

	result := complete(t, r, transport.CompletionRef{Type: "ref/tool", Name: "fs_write"}, transport.CompletionArgument{Name: "path", Value: "/tmp/"}, transport.CompletionContext{Arguments: map[string]string{"clientId": id}})
	if len(result.Values) != 0 {
		t.Fatalf("values = %v, want empty on dispatch failure", result.Values)
	}
}

func TestCompletePath_NonFSToolReturnsEmpty(t *testing.T) {
	r, deviceReg := newTestRegistry(t, nil)
	id := pairDevice(t, deviceReg, "host", true)

	result := complete(t, r, transport.CompletionRef{Type: "ref/tool", Name: "process_list"}, transport.CompletionArgument{Name: "path", Value: "/tmp/"}, transport.CompletionContext{Arguments: map[string]string{"clientId": id}})
	if len(result.Values) != 0 {
		t.Fatalf("values = %v, want empty (process_list has no path argument)", result.Values)
	}
}

func TestCompleteResource_SysinfoClientID(t *testing.T) {
	r, deviceReg := newTestRegistry(t, nil)
	id := pairDevice(t, deviceReg, "host", true)

	result := complete(t, r, transport.CompletionRef{Type: "ref/resource", URI: "sysinfo://{clientId}/overview"}, transport.CompletionArgument{Name: "clientId"}, transport.CompletionContext{})
	if len(result.Values) != 1 || result.Values[0] != id {
		t.Fatalf("values = %v, want [%s]", result.Values, id)
	}
}

func TestCompleteResource_OtherURIsReturnEmpty(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	result := complete(t, r, transport.CompletionRef{Type: "ref/resource", URI: "job://{id}"}, transport.CompletionArgument{Name: "id"}, transport.CompletionContext{})
	if len(result.Values) != 0 {
		t.Fatalf("values = %v, want empty (job IDs are opaque)", result.Values)
	}
}

func TestComplete_UnrecognizedRefTypeReturnsEmptyNotError(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	result := complete(t, r, transport.CompletionRef{Type: "ref/prompt", Name: "diagnose_system"}, transport.CompletionArgument{Name: "clientId"}, transport.CompletionContext{})
	if len(result.Values) != 0 {
		t.Fatalf("values = %v, want empty", result.Values)
	}
}

func TestCapValues_BoundsAndReportsHasMore(t *testing.T) {
	values := make([]string, maxResults+5)
	for i := range values {
		values[i] = "v"
	}
	capped := capValues(values)
	if len(capped.Values) != maxResults {
		t.Fatalf("len = %d, want %d", len(capped.Values), maxResults)
	}
	if capped.Total == nil || *capped.Total != maxResults+5 {
		t.Fatalf("Total = %v, want %d", capped.Total, maxResults+5)
	}
	if !capped.HasMore {
		t.Fatal("HasMore should be true")
	}
}

func TestSplitPathPrefix(t *testing.T) {
	cases := []struct {
		value, dir, prefix string
	}{
		{"", "/", ""},
		{"/tmp/", "/tmp/", ""},
		{"/var/log/ap", "/var/log", "ap"},
		{"foo", "/", "foo"},
	}
	for _, c := range cases {
		dir, prefix := splitPathPrefix(c.value)
		if dir != c.dir || prefix != c.prefix {
			t.Errorf("splitPathPrefix(%q) = (%q, %q), want (%q, %q)", c.value, dir, prefix, c.dir, c.prefix)
		}
	}
}
