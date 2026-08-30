package resources

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/agent"
	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/devices"
	"github.com/CloudKeter/rc-mcp/internal/jobs"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/session"
)

type fakeDispatcher struct {
	fn func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error)
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
	return f.fn(ctx, deviceID, correlationID, tool, sessionID, input, onProgress)
}

type fixture struct {
	reg       *Registry
	devices   *devices.FileRegistry
	jobs      *jobs.MemoryStore
	store     *session.MemoryStore
	auditPath string
}

func newFixture(t *testing.T, dispatch *fakeDispatcher) *fixture {
	t.Helper()
	deviceReg, err := devices.NewFileRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	jobStore := jobs.NewMemoryStore(0, nil)
	store := session.NewMemoryStore(10)
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	if dispatch == nil {
		dispatch = &fakeDispatcher{}
	}
	return &fixture{
		reg:       NewRegistry(deviceReg, jobStore, dispatch, store, auditPath),
		devices:   deviceReg,
		jobs:      jobStore,
		store:     store,
		auditPath: auditPath,
	}
}

func (f *fixture) pairDevice(t *testing.T, online bool) string {
	t.Helper()
	ctx := context.Background()
	pc, err := f.devices.CreatePairingCode(ctx, "host-1")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, _, err := f.devices.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	if err := f.devices.UpdateCapabilities(ctx, device.ID, []string{"sysinfo"}); err != nil {
		t.Fatalf("UpdateCapabilities: %v", err)
	}
	if err := f.devices.SetOnline(ctx, device.ID, online); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	return device.ID
}

func (f *fixture) newSession(t *testing.T) *session.Session {
	t.Helper()
	sess, err := f.store.Create(context.Background())
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	return sess
}

func readJSON(t *testing.T, f *fixture, sess *session.Session, uri string) map[string]any {
	t.Helper()
	contents, rpcErr := f.reg.ReadResource(context.Background(), sess, uri)
	if rpcErr != nil {
		t.Fatalf("ReadResource(%s): %+v", uri, rpcErr)
	}
	if len(contents) != 1 || contents[0].MimeType != "application/json" || contents[0].URI != uri {
		t.Fatalf("contents = %+v", contents)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(contents[0].Text), &out); err != nil {
		t.Fatalf("unmarshal contents: %v", err)
	}
	return out
}

// nextNotification drains sess.EventCh until a notification with method
// arrives, returning its params.
func nextNotification(t *testing.T, sess *session.Session, method string) map[string]any {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-sess.EventCh:
			var msg struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
				t.Fatalf("bad event: %v", err)
			}
			if msg.Method == method {
				return msg.Params
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", method)
		}
	}
}

func TestListResources_AllFive(t *testing.T) {
	f := newFixture(t, nil)
	list := f.reg.ListResources(context.Background())
	if len(list) != 5 {
		t.Fatalf("got %d resources, want 5", len(list))
	}
	uris := map[string]bool{}
	for _, r := range list {
		uris[r.URI] = true
	}
	for _, want := range []string{"clients://list", "job://{id}", "sysinfo://{clientId}/overview", "audit://log", "shell://sessions"} {
		if !uris[want] {
			t.Errorf("missing resource %s", want)
		}
	}
}

func TestClientsList_ReflectsOnlineStatus(t *testing.T) {
	f := newFixture(t, nil)
	id := f.pairDevice(t, true)
	sess := f.newSession(t)

	out := readJSON(t, f, sess, "clients://list")
	clients := out["clients"].([]any)
	if len(clients) != 1 {
		t.Fatalf("clients = %+v", clients)
	}
	c := clients[0].(map[string]any)
	if c["clientId"] != id || c["online"] != true {
		t.Fatalf("client = %+v", c)
	}

	if err := f.devices.SetOnline(context.Background(), id, false); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	out = readJSON(t, f, sess, "clients://list")
	c = out["clients"].([]any)[0].(map[string]any)
	if c["online"] != false {
		t.Fatalf("client should be offline: %+v", c)
	}
}

func TestJobResource_OwnerOnly(t *testing.T) {
	f := newFixture(t, nil)
	owner := f.newSession(t)
	other := f.newSession(t)

	job := &jobs.Job{ID: "job-1", SessionID: owner.ID, ClientID: "dev-1", Tool: "screenshot_watch", Status: jobs.JobStatusRunning}
	if err := f.jobs.Create(job); err != nil {
		t.Fatalf("Create: %v", err)
	}

	out := readJSON(t, f, owner, "job://job-1")
	if out["status"] != "running" || out["id"] != "job-1" {
		t.Fatalf("job = %+v", out)
	}

	if _, rpcErr := f.reg.ReadResource(context.Background(), other, "job://job-1"); rpcErr == nil {
		t.Fatal("another session must not read the job")
	}
	if _, rpcErr := f.reg.ReadResource(context.Background(), owner, "job://nope"); rpcErr == nil {
		t.Fatal("unknown job must error")
	}
}

func TestSysinfo_OfflineAndUnknownDevice(t *testing.T) {
	f := newFixture(t, nil)
	id := f.pairDevice(t, false)
	sess := f.newSession(t)

	_, rpcErr := f.reg.ReadResource(context.Background(), sess, "sysinfo://"+id+"/overview")
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "offline") {
		t.Fatalf("rpcErr = %+v, want offline error", rpcErr)
	}
	_, rpcErr = f.reg.ReadResource(context.Background(), sess, "sysinfo://ghost/overview")
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "Unknown device") {
		t.Fatalf("rpcErr = %+v, want unknown device", rpcErr)
	}
}

func TestSysinfo_OnlineDispatchesRead(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		if tool != "sysinfo_get" {
			t.Errorf("tool = %q", tool)
		}
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{"hostname": "box-1"}}, nil
	}}
	f := newFixture(t, dispatch)
	id := f.pairDevice(t, true)
	sess := f.newSession(t)

	out := readJSON(t, f, sess, "sysinfo://"+id+"/overview")
	if out["hostname"] != "box-1" {
		t.Fatalf("out = %+v", out)
	}
}

func TestAuditLog_NewestFirstPagination(t *testing.T) {
	f := newFixture(t, nil)
	logger, err := audit.NewLogger(f.auditPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()
	for i := 0; i < 5; i++ {
		if err := logger.Write(audit.Entry{Tool: "tool-" + string(rune('a'+i)), Timestamp: time.Now().UTC()}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	sess := f.newSession(t)

	out := readJSON(t, f, sess, "audit://log?limit=2&offset=1")
	entries := out["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	// Newest-first: full order is e,d,c,b,a; offset 1 limit 2 -> d,c.
	if entries[0].(map[string]any)["tool"] != "tool-d" || entries[1].(map[string]any)["tool"] != "tool-c" {
		t.Fatalf("pagination order wrong: %+v", entries)
	}

	if _, rpcErr := f.reg.ReadResource(context.Background(), sess, "audit://log?limit=zero"); rpcErr == nil {
		t.Fatal("bad limit must error")
	}
}

func TestShellSessions_ListsCurrentSessionOnly(t *testing.T) {
	f := newFixture(t, nil)
	sess := f.newSession(t)
	sess.SetShellSession("ss-1", &session.ShellSessionEntry{ClientID: "dev-1", PID: 42, Shell: "/bin/bash", CreatedAt: time.Now().UTC()})

	out := readJSON(t, f, sess, "shell://sessions")
	sessions := out["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %+v", sessions)
	}
	s := sessions[0].(map[string]any)
	if s["shellSessionId"] != "ss-1" || s["clientId"] != "dev-1" || s["pid"] != float64(42) {
		t.Fatalf("session = %+v", s)
	}

	other := f.newSession(t)
	out = readJSON(t, f, other, "shell://sessions")
	if len(out["sessions"].([]any)) != 0 {
		t.Fatal("shell sessions must be scoped to the MCP session")
	}
}

func TestSubscribe_UnknownURIRejected(t *testing.T) {
	f := newFixture(t, nil)
	sess := f.newSession(t)
	if rpcErr := f.reg.SubscribeResource(context.Background(), sess, "bogus://x"); rpcErr == nil {
		t.Fatal("unknown URI must be rejected")
	}
}

func TestNotify_SubscriptionGated(t *testing.T) {
	f := newFixture(t, nil)
	sub := f.newSession(t)
	unsub := f.newSession(t)
	if rpcErr := f.reg.SubscribeResource(context.Background(), sub, "clients://list"); rpcErr != nil {
		t.Fatalf("subscribe: %+v", rpcErr)
	}

	f.reg.BroadcastUpdated("clients://list")

	params := nextNotification(t, sub, "notifications/resources/updated")
	if params["uri"] != "clients://list" {
		t.Fatalf("params = %+v", params)
	}
	select {
	case ev := <-unsub.EventCh:
		t.Fatalf("unsubscribed session received event: %s", ev.Data)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNotifyDeviceChange_UpdatesAndListChanged(t *testing.T) {
	f := newFixture(t, nil)
	sess := f.newSession(t)
	_ = f.reg.SubscribeResource(context.Background(), sess, "clients://list")

	f.reg.NotifyDeviceChange("dev-1")

	if params := nextNotification(t, sess, "notifications/resources/updated"); params["uri"] != "clients://list" {
		t.Fatalf("params = %+v", params)
	}
	nextNotification(t, sess, "notifications/tools/list_changed")
}

func TestNotifyJobUpdated_OwnerSessionOnly(t *testing.T) {
	f := newFixture(t, nil)
	owner := f.newSession(t)
	f.jobs.OnUpdate = f.reg.NotifyJobUpdated

	job := &jobs.Job{ID: "job-9", SessionID: owner.ID, ClientID: "dev-1", Tool: "screenshot_watch", Status: jobs.JobStatusPending}
	if err := f.jobs.Create(job); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = f.reg.SubscribeResource(context.Background(), owner, "job://job-9")

	if err := f.jobs.UpdateStatus("job-9", jobs.JobStatusSucceeded, nil, ""); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if params := nextNotification(t, owner, "notifications/resources/updated"); params["uri"] != "job://job-9" {
		t.Fatalf("params = %+v", params)
	}
}

func TestAuditOnWrite_Broadcasts(t *testing.T) {
	f := newFixture(t, nil)
	sess := f.newSession(t)
	_ = f.reg.SubscribeResource(context.Background(), sess, "audit://log")

	logger, err := audit.NewLogger(f.auditPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()
	logger.OnWrite = f.reg.NotifyAuditEntry

	if err := logger.Write(audit.Entry{Tool: "shell_exec", Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if params := nextNotification(t, sess, "notifications/resources/updated"); params["uri"] != "audit://log" {
		t.Fatalf("params = %+v", params)
	}
}

func TestSysinfoRefresher_PushesWhileOnlineStopsOnUnsubscribe(t *testing.T) {
	f := newFixture(t, nil)
	f.reg.RefreshInterval = 20 * time.Millisecond
	id := f.pairDevice(t, true)
	sess := f.newSession(t)
	uri := "sysinfo://" + id + "/overview"

	if rpcErr := f.reg.SubscribeResource(context.Background(), sess, uri); rpcErr != nil {
		t.Fatalf("subscribe: %+v", rpcErr)
	}
	if params := nextNotification(t, sess, "notifications/resources/updated"); params["uri"] != uri {
		t.Fatalf("params = %+v", params)
	}

	if rpcErr := f.reg.UnsubscribeResource(context.Background(), sess, uri); rpcErr != nil {
		t.Fatalf("unsubscribe: %+v", rpcErr)
	}
	// Drain anything already in flight, then expect silence.
	time.Sleep(50 * time.Millisecond)
	for {
		select {
		case <-sess.EventCh:
		default:
			goto drained
		}
	}
drained:
	select {
	case ev := <-sess.EventCh:
		t.Fatalf("refresher still running after unsubscribe: %s", ev.Data)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestSysinfoRefresher_PausesWhileOffline(t *testing.T) {
	f := newFixture(t, nil)
	f.reg.RefreshInterval = 20 * time.Millisecond
	id := f.pairDevice(t, false)
	sess := f.newSession(t)
	uri := "sysinfo://" + id + "/overview"

	_ = f.reg.SubscribeResource(context.Background(), sess, uri)
	select {
	case ev := <-sess.EventCh:
		t.Fatalf("offline device should not push updates: %s", ev.Data)
	case <-time.After(80 * time.Millisecond):
	}

	if err := f.devices.SetOnline(context.Background(), id, true); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	if params := nextNotification(t, sess, "notifications/resources/updated"); params["uri"] != uri {
		t.Fatalf("params = %+v", params)
	}
}
