package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/champi-ai/rc-mcp/internal/protocol"
)

func setupDispatchTest(t *testing.T) (*Hub, *Bridge, *websocket.Conn, string) {
	t.Helper()
	h, reg, srv := newTestHub(t)

	pc, err := reg.CreatePairingCode(context.Background(), "dispatch-test-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, token, err := reg.ApprovePairing(context.Background(), pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	conn := dialWS(t, srv)
	if err := conn.WriteJSON(protocol.Envelope{
		Type:            protocol.MsgHello,
		ID:              "hello-1",
		ProtocolVersion: protocol.Version,
		Ts:              time.Now().UTC(),
		Payload: protocol.HelloPayload{
			DeviceToken:  token,
			Hostname:     "dispatch-test-host",
			Capabilities: []string{"shell"},
		},
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	ack := readEnvelope(t, conn, 2*time.Second)
	if ack.Type != protocol.MsgHelloAck {
		t.Fatalf("expected hello_ack, got %s", ack.Type)
	}

	deadline := time.Now().Add(2 * time.Second)
	for h.AgentsOnline() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	_ = srv
	return h, NewBridge(h), conn, device.ID
}

func TestBridge_Dispatch_ProgressThenResult(t *testing.T) {
	h, bridge, conn, deviceID := setupDispatchTest(t)
	_ = h

	correlationID := "de0adbee-e29b-41d4-a716-446655440000"

	// Play the agent side in a goroutine: read the dispatch, send a couple
	// of progress messages, then a result.
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		dispatchEnv := readEnvelope(t, conn, 2*time.Second)
		if dispatchEnv.Type != protocol.MsgDispatch {
			t.Errorf("expected dispatch, got %s", dispatchEnv.Type)
			return
		}
		payload, err := decodePayload[protocol.DispatchPayload](dispatchEnv.Payload)
		if err != nil || payload.Tool != "shell_exec" {
			t.Errorf("bad dispatch payload: %+v, err=%v", payload, err)
			return
		}

		_ = conn.WriteJSON(protocol.Envelope{
			Type: protocol.MsgProgress,
			ID:   correlationID,
			Ts:   time.Now().UTC(),
			Payload: protocol.ProgressPayload{
				Tool:    "shell_exec",
				Message: "chunk1",
			},
		})
		_ = conn.WriteJSON(protocol.Envelope{
			Type: protocol.MsgResult,
			ID:   correlationID,
			Ts:   time.Now().UTC(),
			Payload: protocol.ResultPayload{
				Tool:    "shell_exec",
				Output:  map[string]any{"stdout": "hello\n", "exitCode": 0},
				IsError: false,
			},
		})
	}()

	var progressMessages []string
	var mu sync.Mutex
	result, err := bridge.Dispatch(context.Background(), deviceID, correlationID, "shell_exec", "sess-1",
		map[string]any{"command": "echo hello"},
		func(p *protocol.ProgressPayload, bin *BinaryFrame) {
			if p != nil {
				mu.Lock()
				progressMessages = append(progressMessages, p.Message)
				mu.Unlock()
			}
		})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, want false")
	}

	<-agentDone
	mu.Lock()
	defer mu.Unlock()
	if len(progressMessages) != 1 || progressMessages[0] != "chunk1" {
		t.Fatalf("progressMessages = %v, want [chunk1]", progressMessages)
	}
}

func TestBridge_Dispatch_BinaryFrameRouting(t *testing.T) {
	_, bridge, conn, deviceID := setupDispatchTest(t)

	correlationID := "aabbccdd-e29b-41d4-a716-446655440000"
	prefix, err := protocol.CorrelationPrefix(correlationID)
	if err != nil {
		t.Fatalf("CorrelationPrefix: %v", err)
	}

	go func() {
		dispatchEnv := readEnvelope(t, conn, 2*time.Second)
		if dispatchEnv.Type != protocol.MsgDispatch {
			t.Errorf("expected dispatch, got %s", dispatchEnv.Type)
			return
		}

		buf := make([]byte, protocol.BinaryHeaderSize+5)
		protocol.EncodeBinaryHeader(buf, protocol.BinaryHeader{
			CorrelationPrefix: prefix,
			StreamSeq:         1,
			FrameType:         protocol.FrameShellStdout,
		})
		copy(buf[protocol.BinaryHeaderSize:], []byte("hello"))
		_ = conn.WriteMessage(websocket.BinaryMessage, buf)

		_ = conn.WriteJSON(protocol.Envelope{
			Type:    protocol.MsgResult,
			ID:      correlationID,
			Ts:      time.Now().UTC(),
			Payload: protocol.ResultPayload{Tool: "shell_exec"},
		})
	}()

	var gotBinary []byte
	var mu sync.Mutex
	_, err = bridge.Dispatch(context.Background(), deviceID, correlationID, "shell_exec", "sess-1", nil,
		func(p *protocol.ProgressPayload, bin *BinaryFrame) {
			if bin != nil {
				mu.Lock()
				gotBinary = append([]byte(nil), bin.Data...)
				mu.Unlock()
			}
		})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(gotBinary) != "hello" {
		t.Fatalf("gotBinary = %q, want %q", gotBinary, "hello")
	}
}

func TestBridge_Dispatch_ContextCancellationSendsCancel(t *testing.T) {
	_, bridge, conn, deviceID := setupDispatchTest(t)

	correlationID := "cc112233-e29b-41d4-a716-446655440000"
	ctx, cancel := context.WithCancel(context.Background())

	cancelSeen := make(chan struct{})
	go func() {
		dispatchEnv := readEnvelope(t, conn, 2*time.Second)
		if dispatchEnv.Type != protocol.MsgDispatch {
			t.Errorf("expected dispatch, got %s", dispatchEnv.Type)
			return
		}
		cancelEnv := readEnvelope(t, conn, 2*time.Second)
		if cancelEnv.Type != protocol.MsgCancel {
			t.Errorf("expected cancel, got %s", cancelEnv.Type)
			return
		}
		close(cancelSeen)
		// Agent acknowledges with a killed result.
		_ = conn.WriteJSON(protocol.Envelope{
			Type: protocol.MsgResult,
			ID:   correlationID,
			Ts:   time.Now().UTC(),
			Payload: protocol.ResultPayload{
				Tool:   "shell_exec",
				Output: map[string]any{"killed": true},
			},
		})
	}()

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	result, err := bridge.Dispatch(ctx, deviceID, correlationID, "shell_exec", "sess-1", nil, nil)
	select {
	case <-cancelSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("agent never observed a cancel message")
	}
	if err != nil {
		t.Fatalf("Dispatch after graceful cancel-then-result: want nil error, got %v", err)
	}
	out, _ := result.Output.(map[string]any)
	if out["killed"] != true {
		t.Fatalf("result.Output = %+v, want killed=true", result.Output)
	}
}

func TestBridge_Dispatch_DeviceOffline(t *testing.T) {
	h, _, srv := newTestHub(t)
	_ = srv
	bridge := NewBridge(h)

	_, err := bridge.Dispatch(context.Background(), "unknown-device", "id-1", "shell_exec", "sess-1", nil, nil)
	if err != ErrDeviceOffline {
		t.Fatalf("err = %v, want ErrDeviceOffline", err)
	}
}

func TestBridge_ConcurrentDispatches(t *testing.T) {
	_, bridge, conn, deviceID := setupDispatchTest(t)

	const n = 10
	// Agent side: for each dispatch it reads, immediately reply with a
	// result matching the correlation ID.
	go func() {
		for i := 0; i < n; i++ {
			env := readEnvelope(t, conn, 3*time.Second)
			if env.Type != protocol.MsgDispatch {
				t.Errorf("expected dispatch, got %s", env.Type)
				return
			}
			_ = conn.WriteJSON(protocol.Envelope{
				Type:    protocol.MsgResult,
				ID:      env.ID,
				Ts:      time.Now().UTC(),
				Payload: protocol.ResultPayload{Tool: "shell_exec", Output: map[string]any{"n": env.ID}},
			})
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("1122%04d-e29b-41d4-a716-446655440000", i)
			_, err := bridge.Dispatch(context.Background(), deviceID, id, "shell_exec", "sess-1", nil, nil)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Dispatch: %v", err)
		}
	}
}
