package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/devices"
)

func TestHealthHandler_ZeroAgents(t *testing.T) {
	reg, err := devices.NewFileRegistry(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	hub := agent.NewHub(reg, 5*time.Minute)

	req, _ := http.NewRequest(http.MethodGet, "/health", nil)
	rec := newRecorder()
	healthHandler(hub)(rec, req)

	if rec.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.status)
	}
	var resp healthResponse
	if err := json.Unmarshal(rec.body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status field = %q, want %q", resp.Status, "ok")
	}
	if resp.AgentsOnline != 0 {
		t.Errorf("agents_online = %d, want 0", resp.AgentsOnline)
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	for _, k := range []string{"MCP_BIND_ADDR", "ADMIN_BIND_ADDR", "DEVICE_REGISTRY_PATH", "PAIRING_CODE_TTL", "AUTH_TOKEN", "MCP_ALLOWED_ORIGINS", "SSE_REPLAY_BUFFER_SIZE", "MCP_SESSION_IDLE_TIMEOUT", "RC_AUDIT_LOG_PATH", "RC_RATE_LIMIT_SESSION", "RC_RATE_LIMIT_TOOLS", "RC_MAX_CONCURRENT_DISPATCHES", "RC_SHELL_SKIP_CONFIRM", "RC_ELICITATION_TIMEOUT"} {
		_ = os.Unsetenv(k)
	}
	cfg := loadConfig()
	if cfg.mcpBindAddr != "0.0.0.0:8080" {
		t.Errorf("mcpBindAddr = %q, want %q", cfg.mcpBindAddr, "0.0.0.0:8080")
	}
	if cfg.adminBindAddr != "127.0.0.1:9090" {
		t.Errorf("adminBindAddr = %q, want %q", cfg.adminBindAddr, "127.0.0.1:9090")
	}
	if cfg.pairingCodeTTL != 5*time.Minute {
		t.Errorf("pairingCodeTTL = %v, want 5m", cfg.pairingCodeTTL)
	}
}

func TestLoadConfig_Overrides(t *testing.T) {
	t.Setenv("MCP_BIND_ADDR", "127.0.0.1:9999")
	t.Setenv("PAIRING_CODE_TTL", "1m")

	cfg := loadConfig()
	if cfg.mcpBindAddr != "127.0.0.1:9999" {
		t.Errorf("mcpBindAddr = %q, want override", cfg.mcpBindAddr)
	}
	if cfg.pairingCodeTTL != time.Minute {
		t.Errorf("pairingCodeTTL = %v, want 1m", cfg.pairingCodeTTL)
	}
}

func TestRun_RefusesToStartWithoutAuthToken(t *testing.T) {
	cfg := config{
		mcpBindAddr:        "127.0.0.1:18081",
		adminBindAddr:      "127.0.0.1:18091",
		deviceRegistryPath: t.TempDir() + "/devices.json",
		pairingCodeTTL:     5 * time.Minute,
	}
	if err := run(context.Background(), cfg); err == nil {
		t.Fatal("run() with no authToken configured: want error, got nil")
	}
}

func TestRun_HealthEndpointAndGracefulShutdown(t *testing.T) {
	cfg := config{
		mcpBindAddr:        "127.0.0.1:18080",
		adminBindAddr:      "127.0.0.1:18090",
		deviceRegistryPath: t.TempDir() + "/devices.json",
		pairingCodeTTL:     5 * time.Minute,
		shutdownTimeout:    2 * time.Second,
		authToken:          "test-token",
		auditLogPath:       t.TempDir() + "/audit.log",
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	// Wait for the server to come up.
	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + cfg.mcpBindAddr + "/health")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Trigger graceful shutdown.
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run returned error after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of shutdown signal")
	}
}

// recorder is a minimal http.ResponseWriter for tests that don't need the
// full httptest.ResponseRecorder feature set.
type recorder struct {
	status int
	body   []byte
	header http.Header
}

func newRecorder() *recorder {
	return &recorder{status: http.StatusOK, header: http.Header{}}
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return len(b), nil
}
func (r *recorder) WriteHeader(status int) { r.status = status }

func TestLoadConfig_ShellAllowDenylist(t *testing.T) {
	t.Setenv("RC_SHELL_DENYLIST", "rm\\s+-rf\n^sudo\\b")
	t.Setenv("RC_SHELL_ALLOWLIST", "^ls\\b")

	cfg := loadConfig()
	if len(cfg.shellDenylist) != 2 || cfg.shellDenylist[0] != `rm\s+-rf` || cfg.shellDenylist[1] != `^sudo\b` {
		t.Errorf("shellDenylist = %v", cfg.shellDenylist)
	}
	if len(cfg.shellAllowlist) != 1 || cfg.shellAllowlist[0] != `^ls\b` {
		t.Errorf("shellAllowlist = %v", cfg.shellAllowlist)
	}
}

func TestRun_RefusesToStartWithInvalidShellPolicyRegex(t *testing.T) {
	cfg := config{
		mcpBindAddr:        "127.0.0.1:18082",
		adminBindAddr:      "127.0.0.1:18092",
		deviceRegistryPath: t.TempDir() + "/devices.json",
		pairingCodeTTL:     5 * time.Minute,
		authToken:          "test-token",
		auditLogPath:       t.TempDir() + "/audit.log",
		shellDenylist:      []string{"("},
	}
	if err := run(context.Background(), cfg); err == nil {
		t.Fatal("run() with an invalid shell denylist pattern: want error, got nil")
	}
}

func TestLoadConfig_AuditFullArgsAndGlobalFSRoots(t *testing.T) {
	t.Setenv("RC_AUDIT_FULL_ARGS", "true")
	t.Setenv("RC_GLOBAL_FS_ALLOWED_ROOTS", "/srv/data:/var/app")

	cfg := loadConfig()
	if !cfg.auditFullArgs {
		t.Error("auditFullArgs = false, want true")
	}
	if len(cfg.globalFSAllowedRoots) != 2 || cfg.globalFSAllowedRoots[0] != "/srv/data" || cfg.globalFSAllowedRoots[1] != "/var/app" {
		t.Errorf("globalFSAllowedRoots = %v", cfg.globalFSAllowedRoots)
	}
}

func TestLoadConfig_AuditFullArgsDefaultsFalse(t *testing.T) {
	_ = os.Unsetenv("RC_AUDIT_FULL_ARGS")
	_ = os.Unsetenv("RC_GLOBAL_FS_ALLOWED_ROOTS")

	cfg := loadConfig()
	if cfg.auditFullArgs {
		t.Error("auditFullArgs should default to false")
	}
	if cfg.globalFSAllowedRoots != nil {
		t.Errorf("globalFSAllowedRoots should default to nil (unrestricted), got %v", cfg.globalFSAllowedRoots)
	}
}

func TestRun_RefusesToStartWithInvalidGlobalFSRoot(t *testing.T) {
	cfg := config{
		mcpBindAddr:          "127.0.0.1:18083",
		adminBindAddr:        "127.0.0.1:18093",
		deviceRegistryPath:   t.TempDir() + "/devices.json",
		pairingCodeTTL:       5 * time.Minute,
		authToken:            "test-token",
		auditLogPath:         t.TempDir() + "/audit.log",
		globalFSAllowedRoots: []string{"relative/not/absolute"},
	}
	if err := run(context.Background(), cfg); err == nil {
		t.Fatal("run() with a non-absolute global fs root: want error, got nil")
	}
}

func TestLoadConfig_SessionStoreDefaults(t *testing.T) {
	_ = os.Unsetenv("MCP_SESSION_STORE")
	_ = os.Unsetenv("REDIS_ADDR")

	cfg := loadConfig()
	if cfg.sessionStore != sessionStoreMemory {
		t.Errorf("sessionStore = %q, want %q", cfg.sessionStore, sessionStoreMemory)
	}
	if cfg.redisAddr != "localhost:6379" {
		t.Errorf("redisAddr = %q, want default", cfg.redisAddr)
	}
}

func TestLoadConfig_SessionStoreOverride(t *testing.T) {
	t.Setenv("MCP_SESSION_STORE", "redis")
	t.Setenv("REDIS_ADDR", "redis-host:6380")

	cfg := loadConfig()
	if cfg.sessionStore != "redis" {
		t.Errorf("sessionStore = %q, want redis", cfg.sessionStore)
	}
	if cfg.redisAddr != "redis-host:6380" {
		t.Errorf("redisAddr = %q, want redis-host:6380", cfg.redisAddr)
	}
}

func TestRun_RefusesToStartWithInvalidSessionStore(t *testing.T) {
	cfg := config{
		mcpBindAddr:        "127.0.0.1:18084",
		adminBindAddr:      "127.0.0.1:18094",
		deviceRegistryPath: t.TempDir() + "/devices.json",
		pairingCodeTTL:     5 * time.Minute,
		authToken:          "test-token",
		auditLogPath:       t.TempDir() + "/audit.log",
		sessionStore:       "postgres",
	}
	if err := run(context.Background(), cfg); err == nil {
		t.Fatal("run() with an invalid MCP_SESSION_STORE: want error, got nil")
	}
}

func TestRun_RefusesToStartWhenRedisUnreachable(t *testing.T) {
	cfg := config{
		mcpBindAddr:        "127.0.0.1:18085",
		adminBindAddr:      "127.0.0.1:18095",
		deviceRegistryPath: t.TempDir() + "/devices.json",
		pairingCodeTTL:     5 * time.Minute,
		authToken:          "test-token",
		auditLogPath:       t.TempDir() + "/audit.log",
		sessionStore:       sessionStoreRedis,
		// Port 1 should never have anything listening; Ping should fail
		// promptly rather than hanging.
		redisAddr: "127.0.0.1:1",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := run(ctx, cfg); err == nil {
		t.Fatal("run() with an unreachable Redis: want error, got nil")
	}
}

func TestRun_DefaultSessionStoreDoesNotRequireRedis(t *testing.T) {
	// Regression guard: the default (memory) path must never attempt to
	// contact Redis, so a config with no redisAddr set at all still works.
	cfg := config{
		mcpBindAddr:        "127.0.0.1:18086",
		adminBindAddr:      "127.0.0.1:18096",
		deviceRegistryPath: t.TempDir() + "/devices.json",
		pairingCodeTTL:     5 * time.Minute,
		shutdownTimeout:    2 * time.Second,
		authToken:          "test-token",
		auditLogPath:       t.TempDir() + "/audit.log",
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + cfg.mcpBindAddr + "/health")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("server never became healthy: %v", err)
	}
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run() returned error on shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run() did not shut down in time")
	}
}

func TestLoadConfig_AgentLatestVersion(t *testing.T) {
	_ = os.Unsetenv("AGENT_LATEST_VERSION")
	cfg := loadConfig()
	if cfg.agentLatestVersion != "" {
		t.Errorf("agentLatestVersion = %q, want empty by default", cfg.agentLatestVersion)
	}

	t.Setenv("AGENT_LATEST_VERSION", "2.0.0")
	cfg = loadConfig()
	if cfg.agentLatestVersion != "2.0.0" {
		t.Errorf("agentLatestVersion = %q, want 2.0.0", cfg.agentLatestVersion)
	}
}

func TestLoadConfig_ReplicaID(t *testing.T) {
	_ = os.Unsetenv("REPLICA_ID")
	cfg := loadConfig()
	if cfg.replicaID == "" {
		t.Error("replicaID should default to a generated non-empty value")
	}

	t.Setenv("REPLICA_ID", "replica-42")
	cfg = loadConfig()
	if cfg.replicaID != "replica-42" {
		t.Errorf("replicaID = %q, want replica-42", cfg.replicaID)
	}
}

func TestDefaultReplicaID_Unique(t *testing.T) {
	a := defaultReplicaID()
	b := defaultReplicaID()
	if a == b {
		t.Fatal("defaultReplicaID() should not produce the same value twice (random suffix)")
	}
}

func TestRun_RedisMode_StartsCrossReplicaRelay(t *testing.T) {
	// This exercises the wiring path (redisKV present -> ReplicaBridge
	// constructed, hooks wired, goroutines started) against a real Redis
	// look-alike is not available in this test environment; instead it
	// confirms run() still refuses cleanly when Redis is unreachable,
	// which is the only reachable assertion without a live Redis server
	// (the happy-path cross-replica plumbing itself is covered directly
	// in internal/agent's relay_test.go against redisclient.Fake).
	cfg := config{
		mcpBindAddr:        "127.0.0.1:18087",
		adminBindAddr:      "127.0.0.1:18097",
		deviceRegistryPath: t.TempDir() + "/devices.json",
		pairingCodeTTL:     5 * time.Minute,
		authToken:          "test-token",
		auditLogPath:       t.TempDir() + "/audit.log",
		sessionStore:       sessionStoreRedis,
		redisAddr:          "127.0.0.1:1",
		replicaID:          "replica-test",
	}
	if err := run(context.Background(), cfg); err == nil {
		t.Fatal("run() with redis mode + unreachable redis: want error, got nil")
	}
}
