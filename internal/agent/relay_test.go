package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/redisclient"
)

// TestReplicaBridge_CrossReplicaDispatch_EndToEnd is the acceptance test
// for issue #45: a dispatch bridge on "replica A" (no local connection to
// the target device) reaches an agent actually connected to "replica B",
// via the pub/sub relay, and progress/result messages route back
// correctly to the originating call on replica A. Two redisclient.Fake
// handles sharing one underlying state stand in for two server processes
// pointed at the same Redis instance.
func TestReplicaBridge_CrossReplicaDispatch_EndToEnd(t *testing.T) {
	// "Replica B" holds the real agent connection (reusing the existing
	// dispatch test harness: a live Hub + WS-connected fake agent).
	hubB, bridgeLocalB, agentConn, deviceID := setupDispatchTest(t)

	fakeB := redisclient.NewFake()
	fakeA := fakeB.NewHandle() // "replica A"'s client to the same Redis

	bridgeB := &ReplicaBridge{
		Local:     bridgeLocalB,
		Hub:       hubB,
		PubSub:    fakeB,
		Locations: &LocationTracker{KV: fakeB, ReplicaID: "replica-b"},
		ReplicaID: "replica-b",
		Ready:     make(chan struct{}),
	}
	if err := bridgeB.Locations.MarkOnline(context.Background(), deviceID); err != nil {
		t.Fatalf("MarkOnline: %v", err)
	}

	serveCtx, stopServing := context.WithCancel(context.Background())
	defer stopServing()
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- bridgeB.ServeRelayedDispatches(serveCtx) }()
	// Wait for replica B's subscription to actually be active before
	// publishing a request to it -- a plain pub/sub Publish delivers to
	// zero subscribers if it races ahead of Subscribe (real Redis and the
	// Fake both drop it silently in that case), which would otherwise
	// make this test flaky under scheduler contention.
	select {
	case <-bridgeB.Ready:
	case <-time.After(2 * time.Second):
		t.Fatal("replica B's relay never became ready")
	}

	// "Replica A" has no local connection to deviceID at all.
	hubA, _, _ := newTestHub(t)
	bridgeA := &ReplicaBridge{
		Local:     NewBridge(hubA),
		Hub:       hubA,
		PubSub:    fakeA,
		Locations: &LocationTracker{KV: fakeA, ReplicaID: "replica-a"},
		ReplicaID: "replica-a",
	}

	// Play the agent side: read the dispatch (arrives over the real WS
	// connection to replica B, exactly as a local dispatch would), send
	// progress then a result.
	correlationID := "de0adbee-e29b-41d4-a716-446655440030"
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		dispatchEnv := readEnvelope(t, agentConn, 2*time.Second)
		if dispatchEnv.Type != protocol.MsgDispatch {
			t.Errorf("expected dispatch, got %s", dispatchEnv.Type)
			return
		}
		payload, err := decodePayload[protocol.DispatchPayload](dispatchEnv.Payload)
		if err != nil || payload.Tool != "shell_exec" {
			t.Errorf("bad dispatch payload: %+v, err=%v", payload, err)
			return
		}
		_ = agentConn.WriteJSON(protocol.Envelope{
			Type: protocol.MsgProgress, ID: correlationID, Ts: time.Now().UTC(),
			Payload: protocol.ProgressPayload{Tool: "shell_exec", Message: "relayed-chunk"},
		})
		_ = agentConn.WriteJSON(protocol.Envelope{
			Type: protocol.MsgResult, ID: correlationID, Ts: time.Now().UTC(),
			Payload: protocol.ResultPayload{Tool: "shell_exec", Output: map[string]any{"stdout": "hello-from-b\n", "exitCode": 0}},
		})
	}()

	var mu sync.Mutex
	var progressMessages []string
	result, err := bridgeA.Dispatch(context.Background(), deviceID, correlationID, "shell_exec", "sess-1",
		map[string]any{"command": "echo hello"},
		func(p *protocol.ProgressPayload, bin *BinaryFrame) {
			if p != nil {
				mu.Lock()
				progressMessages = append(progressMessages, p.Message)
				mu.Unlock()
			}
		})
	if err != nil {
		t.Fatalf("cross-replica Dispatch: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, want false: %+v", result)
	}
	out, decErr := decodePayload[map[string]any](result.Output)
	if decErr != nil || out["stdout"] != "hello-from-b\n" {
		t.Fatalf("output = %+v, err = %v", out, decErr)
	}

	<-agentDone
	mu.Lock()
	defer mu.Unlock()
	if len(progressMessages) != 1 || progressMessages[0] != "relayed-chunk" {
		t.Fatalf("progressMessages = %v", progressMessages)
	}

	stopServing()
	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Fatalf("ServeRelayedDispatches: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeRelayedDispatches did not stop after ctx cancel")
	}
}

func TestReplicaBridge_Dispatch_DeviceOffline_NoLocationRecord(t *testing.T) {
	hubA, _, _ := newTestHub(t)
	fake := redisclient.NewFake()
	bridgeA := &ReplicaBridge{
		Local:     NewBridge(hubA),
		Hub:       hubA,
		PubSub:    fake,
		Locations: &LocationTracker{KV: fake, ReplicaID: "replica-a"},
		ReplicaID: "replica-a",
	}
	_, err := bridgeA.Dispatch(context.Background(), "ghost-device", "corr-1", "shell_exec", "sess-1", map[string]any{}, nil)
	if err != ErrDeviceOffline {
		t.Fatalf("err = %v, want ErrDeviceOffline", err)
	}
}

func TestReplicaBridge_Dispatch_StaleSelfOwnedLocationRecord(t *testing.T) {
	// This replica's own location record exists (from a connection that
	// has since dropped) but Hub.Connection no longer has it: must not
	// relay a request to itself.
	hubA, _, _ := newTestHub(t)
	fake := redisclient.NewFake()
	tracker := &LocationTracker{KV: fake, ReplicaID: "replica-a"}
	if err := tracker.MarkOnline(context.Background(), "dev-1"); err != nil {
		t.Fatalf("MarkOnline: %v", err)
	}
	bridgeA := &ReplicaBridge{Local: NewBridge(hubA), Hub: hubA, PubSub: fake, Locations: tracker, ReplicaID: "replica-a"}

	_, err := bridgeA.Dispatch(context.Background(), "dev-1", "corr-1", "shell_exec", "sess-1", map[string]any{}, nil)
	if err != ErrDeviceOffline {
		t.Fatalf("err = %v, want ErrDeviceOffline", err)
	}
}

func TestReplicaBridge_Dispatch_OwnerNeverResponds_TimesOut(t *testing.T) {
	hubA, _, _ := newTestHub(t)
	fake := redisclient.NewFake()
	tracker := &LocationTracker{KV: fake, ReplicaID: "replica-a"}
	if err := fake.Set(context.Background(), "rc-mcp:agent-location:dev-1", "replica-ghost", time.Minute); err != nil {
		t.Fatalf("seed location: %v", err)
	}
	bridgeA := &ReplicaBridge{
		Local: NewBridge(hubA), Hub: hubA, PubSub: fake, Locations: tracker, ReplicaID: "replica-a",
		RelayTimeout: 50 * time.Millisecond,
	}
	// No one is subscribed to replica-ghost's request channel or ever
	// publishes a reply -- the relay must time out rather than hang.
	_, err := bridgeA.Dispatch(context.Background(), "dev-1", "corr-1", "shell_exec", "sess-1", map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected a timeout error when the owning replica never responds")
	}
}

func TestLocationTracker_MarkOfflineOnlyClearsOwnRecord(t *testing.T) {
	fake := redisclient.NewFake()
	trackerA := &LocationTracker{KV: fake, ReplicaID: "replica-a"}
	trackerB := &LocationTracker{KV: fake, ReplicaID: "replica-b"}
	ctx := context.Background()

	if err := trackerA.MarkOnline(ctx, "dev-1"); err != nil {
		t.Fatalf("MarkOnline: %v", err)
	}
	// dev-1 reconnects to replica B before replica A's stale disconnect
	// callback runs.
	if err := trackerB.MarkOnline(ctx, "dev-1"); err != nil {
		t.Fatalf("MarkOnline: %v", err)
	}
	trackerA.MarkOffline(ctx, "dev-1") // stale: must not clobber replica B's record

	owner, ok := trackerA.Owner(ctx, "dev-1")
	if !ok || owner != "replica-b" {
		t.Fatalf("owner = (%q, %v), want (replica-b, true)", owner, ok)
	}
}

func TestLocationTracker_Refresh(t *testing.T) {
	fake := redisclient.NewFake()
	tracker := &LocationTracker{KV: fake, ReplicaID: "replica-a", LocationTTL: 30 * time.Millisecond}
	ctx := context.Background()
	if err := tracker.MarkOnline(ctx, "dev-1"); err != nil {
		t.Fatalf("MarkOnline: %v", err)
	}

	tracker.Refresh(ctx, []string{"dev-1"})
	time.Sleep(20 * time.Millisecond)
	tracker.Refresh(ctx, []string{"dev-1"})
	time.Sleep(20 * time.Millisecond)

	if _, ok := tracker.Owner(ctx, "dev-1"); !ok {
		t.Fatal("repeated Refresh should keep the location record alive past its base TTL")
	}
}
