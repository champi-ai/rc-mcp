package agent

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/champi-ai/rc-mcp/internal/devices"
	"github.com/champi-ai/rc-mcp/internal/protocol"
)

// pairingTTL is intentionally generous rather than tight: these tests run
// with several Go test binaries executing concurrently in CI, and a short
// TTL (e.g. 200ms) was observed to expire pairing codes
// before ApprovePairing/NotifyApproved could run, causing flaky failures
// under CPU contention. TestPairingExpiry_SendsErrorAndCloses still waits
// past this TTL to exercise the actual expiry path.
const pairingTTL = 2 * time.Second

func newTestHub(t *testing.T) (*Hub, *devices.FileRegistry, *httptest.Server) {
	t.Helper()
	reg, err := devices.NewFileRegistryWithTTL(t.TempDir()+"/devices.json", pairingTTL)
	if err != nil {
		t.Fatalf("NewFileRegistryWithTTL: %v", err)
	}
	h := NewHub(reg, pairingTTL)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	// Registered after srv.Close so it runs first (LIFO): tear down all
	// live agent connections and wait for their goroutines, so registry
	// writes don't race the TempDir cleanup.
	t.Cleanup(func() { h.Shutdown("test_end") })
	return h, reg, srv
}

func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readEnvelope(t *testing.T, conn *websocket.Conn, timeout time.Duration) protocol.Envelope {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	var env protocol.Envelope
	if err := conn.ReadJSON(&env); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	return env
}

func TestPairingFlow_EndToEnd(t *testing.T) {
	h, reg, srv := newTestHub(t)
	conn := dialWS(t, srv)

	if err := conn.WriteJSON(protocol.Envelope{
		Type: protocol.MsgPairRequest,
		ID:   "corr-1",
		Ts:   time.Now().UTC(),
		Payload: protocol.PairRequestPayload{
			Hostname: "test-host",
		},
	}); err != nil {
		t.Fatalf("write pair_request: %v", err)
	}

	codeEnv := readEnvelope(t, conn, 2*time.Second)
	if codeEnv.Type != protocol.MsgPairCode {
		t.Fatalf("expected pair_code, got %s", codeEnv.Type)
	}
	codePayload, err := decodePayload[protocol.PairCodePayload](codeEnv.Payload)
	if err != nil {
		t.Fatalf("decode pair_code payload: %v", err)
	}
	if codePayload.Code == "" {
		t.Fatal("expected a non-empty pairing code")
	}

	// Simulate the admin API approving the code.
	device, token, err := reg.ApprovePairing(context.Background(), codePayload.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	if !h.NotifyApproved(codePayload.Code, device, token) {
		t.Fatal("NotifyApproved: no waiting connection found")
	}

	approvedEnv := readEnvelope(t, conn, 2*time.Second)
	if approvedEnv.Type != protocol.MsgPairApproved {
		t.Fatalf("expected pair_approved, got %s", approvedEnv.Type)
	}
	approvedPayload, err := decodePayload[protocol.PairApprovedPayload](approvedEnv.Payload)
	if err != nil {
		t.Fatalf("decode pair_approved payload: %v", err)
	}
	if approvedPayload.DeviceToken != token {
		t.Errorf("device token mismatch: got %q want %q", approvedPayload.DeviceToken, token)
	}
	if approvedPayload.DeviceID != device.ID {
		t.Errorf("device id mismatch: got %q want %q", approvedPayload.DeviceID, device.ID)
	}

	deadline := time.Now().Add(2 * time.Second)
	for h.AgentsOnline() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.AgentsOnline() != 1 {
		t.Fatalf("expected 1 agent online, got %d", h.AgentsOnline())
	}
}

func TestReconnectWithPersistedToken(t *testing.T) {
	h, reg, srv := newTestHub(t)

	pc, err := reg.CreatePairingCode(context.Background(), "test-host")
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
		ID:              "corr-2",
		ProtocolVersion: protocol.Version,
		Ts:              time.Now().UTC(),
		Payload: protocol.HelloPayload{
			DeviceToken:  token,
			Hostname:     "test-host",
			Capabilities: []string{"shell"},
		},
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	ackEnv := readEnvelope(t, conn, 2*time.Second)
	if ackEnv.Type != protocol.MsgHelloAck {
		t.Fatalf("expected hello_ack, got %s", ackEnv.Type)
	}
	ack, err := decodePayload[protocol.HelloAckPayload](ackEnv.Payload)
	if err != nil {
		t.Fatalf("decode hello_ack payload: %v", err)
	}
	if ack.DeviceID != device.ID {
		t.Errorf("device id mismatch: got %q want %q", ack.DeviceID, device.ID)
	}

	deadline := time.Now().Add(2 * time.Second)
	for h.AgentsOnline() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.AgentsOnline() != 1 {
		t.Fatalf("expected 1 agent online, got %d", h.AgentsOnline())
	}
}

func TestHello_InvalidToken_AuthFailed(t *testing.T) {
	_, _, srv := newTestHub(t)
	conn := dialWS(t, srv)

	if err := conn.WriteJSON(protocol.Envelope{
		Type:            protocol.MsgHello,
		ID:              "corr-3",
		ProtocolVersion: protocol.Version,
		Ts:              time.Now().UTC(),
		Payload: protocol.HelloPayload{
			DeviceToken: "not-a-real-token",
			Hostname:    "test-host",
		},
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	env := readEnvelope(t, conn, 2*time.Second)
	if env.Type != protocol.MsgError {
		t.Fatalf("expected error, got %s", env.Type)
	}
	errPayload, err := decodePayload[protocol.ErrorPayload](env.Payload)
	if err != nil {
		t.Fatalf("decode error payload: %v", err)
	}
	if errPayload.Code != "auth_failed" {
		t.Errorf("error code = %q, want %q", errPayload.Code, "auth_failed")
	}
}

func TestHello_VersionMismatch(t *testing.T) {
	_, _, srv := newTestHub(t)
	conn := dialWS(t, srv)

	if err := conn.WriteJSON(protocol.Envelope{
		Type:            protocol.MsgHello,
		ID:              "corr-4",
		ProtocolVersion: "99",
		Ts:              time.Now().UTC(),
		Payload: protocol.HelloPayload{
			DeviceToken: "irrelevant",
			Hostname:    "test-host",
		},
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	env := readEnvelope(t, conn, 2*time.Second)
	if env.Type != protocol.MsgError {
		t.Fatalf("expected error, got %s", env.Type)
	}
	errPayload, err := decodePayload[protocol.ErrorPayload](env.Payload)
	if err != nil {
		t.Fatalf("decode error payload: %v", err)
	}
	if errPayload.Code != "version_mismatch" {
		t.Errorf("error code = %q, want %q", errPayload.Code, "version_mismatch")
	}
}

func TestPairingExpiry_SendsErrorAndCloses(t *testing.T) {
	_, _, srv := newTestHub(t) // pairingTTL from newTestHub
	conn := dialWS(t, srv)

	if err := conn.WriteJSON(protocol.Envelope{
		Type: protocol.MsgPairRequest,
		ID:   "corr-5",
		Ts:   time.Now().UTC(),
		Payload: protocol.PairRequestPayload{
			Hostname: "test-host",
		},
	}); err != nil {
		t.Fatalf("write pair_request: %v", err)
	}

	// Drain pair_code.
	codeEnv := readEnvelope(t, conn, 2*time.Second)
	if codeEnv.Type != protocol.MsgPairCode {
		t.Fatalf("expected pair_code, got %s", codeEnv.Type)
	}

	// Wait past the TTL without approving.
	expiredEnv := readEnvelope(t, conn, pairingTTL+3*time.Second)
	if expiredEnv.Type != protocol.MsgError {
		t.Fatalf("expected error, got %s", expiredEnv.Type)
	}
	errPayload, err := decodePayload[protocol.ErrorPayload](expiredEnv.Payload)
	if err != nil {
		t.Fatalf("decode error payload: %v", err)
	}
	if errPayload.Code != "pairing_expired" {
		t.Errorf("error code = %q, want %q", errPayload.Code, "pairing_expired")
	}
}

func TestHelloAck_AdvertisesConfiguredLatestAgentVersion(t *testing.T) {
	h, reg, srv := newTestHub(t)
	h.LatestAgentVersion = "1.2.3"

	pc, err := reg.CreatePairingCode(context.Background(), "test-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	_, token, err := reg.ApprovePairing(context.Background(), pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	conn := dialWS(t, srv)
	if err := conn.WriteJSON(protocol.Envelope{
		Type:            protocol.MsgHello,
		ID:              "corr-latest",
		ProtocolVersion: protocol.Version,
		Ts:              time.Now().UTC(),
		Payload:         protocol.HelloPayload{DeviceToken: token, Hostname: "test-host"},
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	ackEnv := readEnvelope(t, conn, 2*time.Second)
	ack, err := decodePayload[protocol.HelloAckPayload](ackEnv.Payload)
	if err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	if ack.LatestAgentVersion != "1.2.3" {
		t.Fatalf("LatestAgentVersion = %q, want %q", ack.LatestAgentVersion, "1.2.3")
	}
}

func TestHelloAck_EmptyLatestAgentVersionByDefault(t *testing.T) {
	_, reg, srv := newTestHub(t)

	pc, err := reg.CreatePairingCode(context.Background(), "test-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	_, token, err := reg.ApprovePairing(context.Background(), pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	conn := dialWS(t, srv)
	if err := conn.WriteJSON(protocol.Envelope{
		Type:            protocol.MsgHello,
		ID:              "corr-default",
		ProtocolVersion: protocol.Version,
		Ts:              time.Now().UTC(),
		Payload:         protocol.HelloPayload{DeviceToken: token, Hostname: "test-host"},
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	ackEnv := readEnvelope(t, conn, 2*time.Second)
	ack, err := decodePayload[protocol.HelloAckPayload](ackEnv.Payload)
	if err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	if ack.LatestAgentVersion != "" {
		t.Fatalf("LatestAgentVersion = %q, want empty by default", ack.LatestAgentVersion)
	}
}
