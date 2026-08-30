package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/champi-ai/rc-mcp/internal/protocol"
)

// connectAgent dials and completes the hello handshake, returning the WS
// connection and the hello_ack payload.
func connectAgent(t *testing.T, h *Hub, srvURL, token string) (*websocket.Conn, protocol.HelloAckPayload) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(srvURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.WriteJSON(protocol.Envelope{
		Type:            protocol.MsgHello,
		ID:              "hello-1",
		ProtocolVersion: protocol.Version,
		Ts:              time.Now().UTC(),
		Payload:         protocol.HelloPayload{DeviceToken: token, Hostname: "reconnect-host", Capabilities: []string{"shell"}},
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	ack := readEnvelope(t, conn, 2*time.Second)
	if ack.Type != protocol.MsgHelloAck {
		t.Fatalf("expected hello_ack, got %s", ack.Type)
	}
	payload, err := decodePayload[protocol.HelloAckPayload](ack.Payload)
	if err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	waitOnline(t, h, 1)
	return conn, payload
}

func waitOnline(t *testing.T, h *Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.AgentsOnline() != want {
		if time.Now().After(deadline) {
			t.Fatalf("AgentsOnline = %d, want %d", h.AgentsOnline(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const reconnectCorrelation = "de0adbee-e29b-41d4-a716-446655440001"

func TestReconnect_WithinGrace_ResumesDispatch(t *testing.T) {
	h, reg, srv := newTestHub(t)
	h.ReconnectGracePeriod = 5 * time.Second
	pc, _ := reg.CreatePairingCode(context.Background(), "host")
	device, token, err := reg.ApprovePairing(context.Background(), pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	bridge := NewBridge(h)
	wsURL := "ws" + srv.URL[4:] + "/agent/ws"

	conn, ack := connectAgent(t, h, wsURL, token)
	if ack.Resume {
		t.Fatal("first connect must not be a resume")
	}

	// Start a dispatch; the agent reads it, then abruptly drops.
	type dispatchResult struct {
		result protocol.ResultPayload
		err    error
	}
	resultCh := make(chan dispatchResult, 1)
	go func() {
		r, err := bridge.Dispatch(context.Background(), device.ID, reconnectCorrelation, "shell_exec", "sess-1", map[string]any{"command": "sleep"}, nil)
		resultCh <- dispatchResult{r, err}
	}()

	env := readEnvelope(t, conn, 2*time.Second)
	if env.Type != protocol.MsgDispatch {
		t.Fatalf("expected dispatch, got %s", env.Type)
	}
	conn.Close()
	waitOnline(t, h, 0)

	select {
	case r := <-resultCh:
		t.Fatalf("dispatch failed during grace period: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	// Reconnect within the grace period: hello_ack must carry resume:true
	// and the original dispatch must complete via the new connection.
	conn2, ack2 := connectAgent(t, h, wsURL, token)
	if !ack2.Resume {
		t.Fatal("reconnect within grace must set resume:true")
	}
	if err := conn2.WriteJSON(protocol.Envelope{
		Type: protocol.MsgResult, ID: reconnectCorrelation, Ts: time.Now().UTC(),
		Payload: protocol.ResultPayload{Tool: "shell_exec", Output: map[string]any{"stdout": "resumed"}},
	}); err != nil {
		t.Fatalf("write result: %v", err)
	}

	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("Dispatch: %v", r.err)
		}
		out, _ := decodePayload[map[string]any](r.result.Output)
		if out["stdout"] != "resumed" {
			t.Fatalf("result = %+v", r.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not complete after resume")
	}
}

func TestReconnect_AfterGrace_DispatchFailsWithConnectionClosed(t *testing.T) {
	h, reg, srv := newTestHub(t)
	h.ReconnectGracePeriod = 100 * time.Millisecond
	pc, _ := reg.CreatePairingCode(context.Background(), "host")
	device, token, err := reg.ApprovePairing(context.Background(), pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	bridge := NewBridge(h)
	wsURL := "ws" + srv.URL[4:] + "/agent/ws"

	conn, _ := connectAgent(t, h, wsURL, token)

	errCh := make(chan error, 1)
	go func() {
		_, err := bridge.Dispatch(context.Background(), device.ID, reconnectCorrelation, "shell_exec", "sess-1", map[string]any{"command": "sleep"}, nil)
		errCh <- err
	}()
	if env := readEnvelope(t, conn, 2*time.Second); env.Type != protocol.MsgDispatch {
		t.Fatalf("expected dispatch, got %s", env.Type)
	}
	conn.Close()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrConnectionClosed) {
			t.Fatalf("err = %v, want ErrConnectionClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not fail after grace expiry")
	}

	// Reconnecting after expiry is a fresh start, not a resume.
	_, ack := connectAgent(t, h, wsURL, token)
	if ack.Resume {
		t.Fatal("reconnect after grace expiry must not set resume:true")
	}
}

func TestRevokeDevice_ClosesConnectionAndExpiresState(t *testing.T) {
	h, reg, srv := newTestHub(t)
	h.ReconnectGracePeriod = 5 * time.Second
	pc, _ := reg.CreatePairingCode(context.Background(), "host")
	device, token, err := reg.ApprovePairing(context.Background(), pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	bridge := NewBridge(h)
	wsURL := "ws" + srv.URL[4:] + "/agent/ws"

	conn, _ := connectAgent(t, h, wsURL, token)

	errCh := make(chan error, 1)
	go func() {
		_, err := bridge.Dispatch(context.Background(), device.ID, reconnectCorrelation, "shell_exec", "sess-1", map[string]any{"command": "sleep"}, nil)
		errCh <- err
	}()
	if env := readEnvelope(t, conn, 2*time.Second); env.Type != protocol.MsgDispatch {
		t.Fatalf("expected dispatch, got %s", env.Type)
	}

	if err := reg.Revoke(context.Background(), device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	h.RevokeDevice(device.ID)

	// The agent observes a "close" envelope with reason "revoked" (or the
	// raw WS close, depending on timing).
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var sawRevoked bool
	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			break
		}
		if env.Type == protocol.MsgClose {
			if p, err := decodePayload[protocol.ClosePayload](env.Payload); err == nil && p.Reason == "revoked" {
				sawRevoked = true
			}
		}
	}
	if !sawRevoked {
		t.Error("agent did not receive close(reason=revoked) envelope")
	}

	// In-flight dispatch fails immediately (no grace period for revoked
	// devices).
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrConnectionClosed) {
			t.Fatalf("err = %v, want ErrConnectionClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch not torn down on revocation")
	}
	waitOnline(t, h, 0)

	// The revoked token no longer authenticates.
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn2.Close()
	_ = conn2.WriteJSON(protocol.Envelope{
		Type: protocol.MsgHello, ID: "hello-2", ProtocolVersion: protocol.Version, Ts: time.Now().UTC(),
		Payload: protocol.HelloPayload{DeviceToken: token, Hostname: "host"},
	})
	env := readEnvelope(t, conn2, 2*time.Second)
	if env.Type != protocol.MsgError {
		t.Fatalf("expected auth error, got %s", env.Type)
	}
	if p, _ := decodePayload[protocol.ErrorPayload](env.Payload); p.Code != "auth_failed" {
		t.Fatalf("error payload = %+v, want auth_failed", p)
	}
}
