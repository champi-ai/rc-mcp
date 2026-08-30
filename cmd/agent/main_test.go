package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/CloudKeter/rc-mcp/agent/client"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
)

func TestLoadConfig_RequiresServerURL(t *testing.T) {
	os.Unsetenv("AGENT_SERVER_URL")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected an error when AGENT_SERVER_URL is unset")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	t.Setenv("AGENT_SERVER_URL", "wss://example.invalid/agent/ws")
	os.Unsetenv("AGENT_TOKEN_PATH")
	os.Unsetenv("AGENT_CAPABILITIES")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.serverURL != "wss://example.invalid/agent/ws" {
		t.Errorf("serverURL = %q", cfg.serverURL)
	}
	if !strings.HasSuffix(cfg.tokenPath, filepath.Join(".rc-mcp", "agent-token")) {
		t.Errorf("tokenPath = %q, want default suffix", cfg.tokenPath)
	}
	want := []string{"shell", "screenshot", "filesystem", "process", "sysinfo"}
	if len(cfg.capabilities) != len(want) {
		t.Fatalf("capabilities = %v, want %v", cfg.capabilities, want)
	}
	for i := range want {
		if cfg.capabilities[i] != want[i] {
			t.Errorf("capabilities[%d] = %q, want %q", i, cfg.capabilities[i], want[i])
		}
	}
}

func TestLoadConfig_Overrides(t *testing.T) {
	t.Setenv("AGENT_SERVER_URL", "wss://example.invalid/agent/ws")
	t.Setenv("AGENT_TOKEN_PATH", "/tmp/custom-token")
	t.Setenv("AGENT_CAPABILITIES", "shell, sysinfo")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.tokenPath != "/tmp/custom-token" {
		t.Errorf("tokenPath = %q", cfg.tokenPath)
	}
	if len(cfg.capabilities) != 2 || cfg.capabilities[0] != "shell" || cfg.capabilities[1] != "sysinfo" {
		t.Errorf("capabilities = %v", cfg.capabilities)
	}
}

// testServer runs a minimal agent-ws-like server for lifecycle tests: it
// handles both hello (existing token) and pair_request (first run).
func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}

		switch env.Type {
		case protocol.MsgHello:
			_ = conn.WriteJSON(protocol.Envelope{
				Type: protocol.MsgHelloAck,
				Ts:   time.Now().UTC(),
				Payload: protocol.HelloAckPayload{
					DeviceID: "device-existing",
					Resume:   false,
				},
			})
		case protocol.MsgPairRequest:
			_ = conn.WriteJSON(protocol.Envelope{
				Type: protocol.MsgPairCode,
				Ts:   time.Now().UTC(),
				Payload: protocol.PairCodePayload{
					Code:      "WXYZ-9876",
					ExpiresAt: time.Now().Add(5 * time.Minute),
				},
			})
			_ = conn.WriteJSON(protocol.Envelope{
				Type: protocol.MsgPairApproved,
				Ts:   time.Now().UTC(),
				Payload: protocol.PairApprovedPayload{
					DeviceID:    "device-new",
					DeviceToken: "brand-new-token",
				},
			})
		default:
			return
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
}

func testWSURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestRun_NoToken_RunsPairingFlow(t *testing.T) {
	srv := testServer(t)
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "agent-token")
	cfg := config{
		serverURL:    testWSURL(srv),
		tokenPath:    tokenPath,
		capabilities: []string{"shell"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfg) }()

	// Wait for the token to be persisted, proving pairing ran.
	deadline := time.Now().Add(2 * time.Second)
	var token string
	var ok bool
	for time.Now().Before(deadline) {
		var err error
		token, ok, err = client.LoadToken(tokenPath)
		if err != nil {
			t.Fatalf("LoadToken: %v", err)
		}
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatal("expected a token to be persisted after pairing")
	}
	if token != "brand-new-token" {
		t.Errorf("token = %q, want %q", token, "brand-new-token")
	}

	cancel()
	<-errCh
}

func TestRun_ExistingToken_ConnectsWithoutPairing(t *testing.T) {
	srv := testServer(t)
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "agent-token")
	if err := client.SaveToken(tokenPath, "already-have-this-token"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	cfg := config{
		serverURL:    testWSURL(srv),
		tokenPath:    tokenPath,
		capabilities: []string{"shell"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfg) }()

	// Give the connection a moment to establish, then verify the token on
	// disk is unchanged (no pairing occurred).
	time.Sleep(200 * time.Millisecond)
	token, ok, err := client.LoadToken(tokenPath)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if !ok || token != "already-have-this-token" {
		t.Fatalf("token changed or missing: ok=%v token=%q", ok, token)
	}

	cancel()
	<-errCh
}

func TestMaybeAutoUpdate_DisabledByDefault_NoNetworkCall(t *testing.T) {
	// No httptest server is set up at all -- if maybeAutoUpdate attempted
	// a real download it would fail/hang against a bogus URL. Since
	// autoUpdate is false, it must return immediately without trying.
	cfg := config{autoUpdate: false, updateBaseURL: "http://127.0.0.1:1/unreachable"}
	maybeAutoUpdate(context.Background(), cfg, "9.9.9")
}

func TestMaybeAutoUpdate_SameVersion_NoOp(t *testing.T) {
	cfg := config{autoUpdate: true, updateBaseURL: "http://127.0.0.1:1/unreachable"}
	maybeAutoUpdate(context.Background(), cfg, version) // latest == running version
}

func TestMaybeAutoUpdate_EmptyLatestVersion_NoOp(t *testing.T) {
	cfg := config{autoUpdate: true, updateBaseURL: "http://127.0.0.1:1/unreachable"}
	maybeAutoUpdate(context.Background(), cfg, "")
}

func TestLoadConfig_AutoUpdateDefaults(t *testing.T) {
	t.Setenv("AGENT_SERVER_URL", "wss://example.invalid/agent/ws")
	os.Unsetenv("AGENT_AUTO_UPDATE")
	os.Unsetenv("AGENT_UPDATE_BASE_URL")
	os.Unsetenv("AGENT_SYSTEMD_UNIT")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.autoUpdate {
		t.Error("autoUpdate should default to false")
	}
	if cfg.updateBaseURL != "https://github.com/CloudKeter/rc-mcp/releases/download" {
		t.Errorf("updateBaseURL = %q", cfg.updateBaseURL)
	}
	if cfg.systemdUnit != "rc-mcp-agent" {
		t.Errorf("systemdUnit = %q", cfg.systemdUnit)
	}
}

func TestLoadConfig_AutoUpdateOverrides(t *testing.T) {
	t.Setenv("AGENT_SERVER_URL", "wss://example.invalid/agent/ws")
	t.Setenv("AGENT_AUTO_UPDATE", "true")
	t.Setenv("AGENT_UPDATE_BASE_URL", "https://mirror.example.invalid/releases")
	t.Setenv("AGENT_SYSTEMD_UNIT", "custom-agent-unit")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.autoUpdate {
		t.Error("autoUpdate should be true")
	}
	if cfg.updateBaseURL != "https://mirror.example.invalid/releases" {
		t.Errorf("updateBaseURL = %q", cfg.updateBaseURL)
	}
	if cfg.systemdUnit != "custom-agent-unit" {
		t.Errorf("systemdUnit = %q", cfg.systemdUnit)
	}
}
