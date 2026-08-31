// Command rc-mcp-server is the relay-hub server entry point: it starts the
// agent WebSocket / health HTTP listener, the localhost-only admin API,
// wires the device registry through both, and handles graceful shutdown.
// See docs/specs/backend.md Section 8 and Section 15.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/champi-ai/rc-mcp/internal/admin"
	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/audit"
	"github.com/champi-ai/rc-mcp/internal/devices"
	"github.com/champi-ai/rc-mcp/internal/fsroot"
	"github.com/champi-ai/rc-mcp/internal/jobs"
	"github.com/champi-ai/rc-mcp/internal/mcp/completions"
	"github.com/champi-ai/rc-mcp/internal/mcp/prompts"
	"github.com/champi-ai/rc-mcp/internal/mcp/resources"
	"github.com/champi-ai/rc-mcp/internal/mcp/tools"
	"github.com/champi-ai/rc-mcp/internal/redisclient"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/shellpolicy"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

const shutdownTimeout = 30 * time.Second

// MCP_SESSION_STORE values (Section 7, Section 19: "multi-replica").
// sessionStoreMemory (the default) keeps the existing single-instance
// behavior unaffected; sessionStoreRedis switches both the session store
// and the device registry to their Redis-backed implementations, sharing
// state across every replica pointed at the same Redis instance.
const (
	sessionStoreMemory = "memory"
	sessionStoreRedis  = "redis"
)

// version is stamped at build time via -ldflags "-X main.version=...", set
// by the release pipeline to the tagged release version ("dev" for a
// local/untagged build). See docs/operations/server-releases.md.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("rc-mcp-server: version %s", version)

	if err := run(ctx, loadConfig()); err != nil {
		log.Fatalf("rc-mcp-server: %v", err)
	}
}

// run wires up dependencies, starts both HTTP listeners, and blocks until
// ctx is cancelled (by a signal in main, or directly by a test), at which
// point it drains agents and shuts down within shutdownTimeout.
func run(ctx context.Context, cfg config) error {
	if cfg.authToken == "" {
		return errors.New("AUTH_TOKEN is required (no anonymous mode); refusing to start")
	}

	// An empty sessionStore (e.g. a config literal built directly by a
	// test, bypassing loadConfig's default) is treated as the memory
	// default rather than a validation error.
	sessionStore := cfg.sessionStore
	if sessionStore == "" {
		sessionStore = sessionStoreMemory
	}
	if sessionStore != sessionStoreMemory && sessionStore != sessionStoreRedis {
		return fmt.Errorf("MCP_SESSION_STORE must be %q or %q, got %q", sessionStoreMemory, sessionStoreRedis, cfg.sessionStore)
	}

	var redisKV redisclient.KVStore
	if sessionStore == sessionStoreRedis {
		redisKV = redisclient.New(cfg.redisAddr)
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := redisKV.Ping(pingCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("MCP_SESSION_STORE=redis but Redis at %s is unreachable: %w", cfg.redisAddr, err)
		}
		defer redisKV.Close()
	}

	var registry devices.DeviceRegistry
	if redisKV != nil {
		registry = devices.NewRedisRegistry(redisKV, cfg.pairingCodeTTL)
	} else {
		fileRegistry, err := devices.NewFileRegistryWithTTL(cfg.deviceRegistryPath, cfg.pairingCodeTTL)
		if err != nil {
			return err
		}
		registry = fileRegistry
	}

	auditLogger, err := audit.NewLogger(cfg.auditLogPath)
	if err != nil {
		return err
	}
	auditLogger.FullArgs = cfg.auditFullArgs
	defer func() {
		if err := auditLogger.Close(); err != nil {
			log.Printf("rc-mcp-server: failed to close audit log cleanly: %v", err)
		}
	}()

	shellPolicy, err := shellpolicy.New(cfg.shellDenylist, cfg.shellAllowlist)
	if err != nil {
		return err
	}
	globalFSRoots, err := fsroot.New(cfg.globalFSAllowedRoots)
	if err != nil {
		return err
	}

	hub := agent.NewHub(registry, cfg.pairingCodeTTL)
	hub.ReconnectGracePeriod = cfg.reconnectGracePeriod
	hub.LatestAgentVersion = cfg.agentLatestVersion
	adminAPI := admin.NewAPI(registry, hub)
	adminAPI.AuditPath = cfg.auditLogPath

	// dispatchBridge is used everywhere a tool/resource/prompt/completion
	// needs to reach an agent. In single-instance mode it is the plain
	// local Bridge (Section 8); in multi-replica mode (MCP_SESSION_STORE=
	// redis) it is a ReplicaBridge that additionally relays a dispatch via
	// Redis Pub/Sub when the target device is connected to a different
	// replica (Section 10, Section 19).
	localBridge := agent.NewBridge(hub)
	var dispatchBridge tools.ShellDispatcher = localBridge
	if redisKV != nil {
		ps, ok := redisKV.(redisclient.PubSub)
		if !ok {
			return errors.New("MCP_SESSION_STORE=redis: redis client does not support pub/sub (internal error)")
		}
		replicaBridge := &agent.ReplicaBridge{
			Local:     localBridge,
			Hub:       hub,
			PubSub:    ps,
			Locations: &agent.LocationTracker{KV: redisKV, ReplicaID: cfg.replicaID},
			ReplicaID: cfg.replicaID,
			Ready:     make(chan struct{}),
		}
		hub.OnLocalPresenceChange = func(deviceID string, online bool) {
			presenceCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if online {
				if err := replicaBridge.Locations.MarkOnline(presenceCtx, deviceID); err != nil {
					log.Printf("rc-mcp-server: failed to record device location for %s: %v", deviceID, err)
				}
			} else {
				replicaBridge.Locations.MarkOffline(presenceCtx, deviceID)
			}
		}
		relayCtx, stopRelay := context.WithCancel(context.Background())
		defer stopRelay()
		go func() {
			if err := replicaBridge.ServeRelayedDispatches(relayCtx); err != nil {
				log.Printf("rc-mcp-server: cross-replica dispatch relay stopped: %v", err)
			}
		}()
		// Wait for the relay subscription to actually be active before
		// this replica starts accepting MCP traffic: a dispatch relayed
		// to us before we're listening would be silently dropped (Redis
		// Pub/Sub delivers only to already-active subscribers).
		select {
		case <-replicaBridge.Ready:
		case <-time.After(10 * time.Second):
			return errors.New("cross-replica dispatch relay did not become ready in time")
		}
		go replicaBridge.RunLocationHeartbeat(relayCtx, 0)
		dispatchBridge = replicaBridge
		log.Printf("rc-mcp-server: cross-replica dispatch routing active (replica id: %s)", cfg.replicaID)
	}

	var store session.SessionStore
	if redisKV != nil {
		store = session.NewRedisStore(redisKV, cfg.sseReplayBufferSize, cfg.sessionIdleTimeout)
	} else {
		store = session.NewMemoryStore(cfg.sseReplayBufferSize)
	}
	idleExpiryCtx, stopIdleExpiry := context.WithCancel(context.Background())
	defer stopIdleExpiry()
	go session.RunIdleExpiry(idleExpiryCtx, store, cfg.sessionIdleTimeout)

	toolRegistry := tools.NewRegistry(registry)
	toolRegistry.Register(tools.NewShellExecDefinition(tools.ShellExecDeps{
		Bridge:             dispatchBridge,
		Audit:              auditLogger,
		SkipConfirm:        cfg.shellSkipConfirm,
		ElicitationTimeout: cfg.elicitationTimeout,
		Policy:             shellPolicy,
	}))
	toolRegistry.Register(tools.NewSysinfoGetDefinition(tools.SysinfoDeps{
		Bridge: dispatchBridge,
		Audit:  auditLogger,
	}))
	processDeps := tools.ProcessDeps{
		Bridge:             dispatchBridge,
		Audit:              auditLogger,
		SkipConfirm:        cfg.processSkipConfirm,
		ElicitationTimeout: cfg.elicitationTimeout,
	}
	toolRegistry.Register(tools.NewProcessListDefinition(processDeps))
	toolRegistry.Register(tools.NewProcessInfoDefinition(processDeps))
	toolRegistry.Register(tools.NewProcessSignalDefinition(processDeps))
	fsDeps := tools.FSDeps{
		Bridge:             dispatchBridge,
		Audit:              auditLogger,
		SkipConfirm:        cfg.fsSkipConfirm,
		ElicitationTimeout: cfg.elicitationTimeout,
		GlobalRoots:        globalFSRoots,
	}
	toolRegistry.Register(tools.NewFSReadDefinition(fsDeps))
	toolRegistry.Register(tools.NewFSWriteDefinition(fsDeps))
	toolRegistry.Register(tools.NewFSListDefinition(fsDeps))
	toolRegistry.Register(tools.NewFSDeleteDefinition(fsDeps))
	toolRegistry.Register(tools.NewFSStatDefinition(fsDeps))
	watchCancels := tools.NewWatchCancels()
	jobStore := jobs.NewMemoryStore(0, watchCancels.Cancel)
	resourceRegistry := resources.NewRegistry(registry, jobStore, dispatchBridge, store, cfg.auditLogPath)
	hub.OnDeviceChange = resourceRegistry.NotifyDeviceChange
	jobStore.OnUpdate = resourceRegistry.NotifyJobUpdated
	auditLogger.OnWrite = resourceRegistry.NotifyAuditEntry

	shellSessionDeps := tools.ShellSessionDeps{
		Bridge:             dispatchBridge,
		Audit:              auditLogger,
		SkipConfirm:        cfg.shellSkipConfirm,
		ConfirmEveryWrite:  cfg.shellConfirmEveryWrite,
		MaxSessions:        cfg.maxShellSessions,
		ElicitationTimeout: cfg.elicitationTimeout,
		Policy:             shellPolicy,
		NotifySessionsChanged: func(sess *session.Session) {
			resourceRegistry.NotifySessionUpdated(sess, "shell://sessions")
		},
	}
	toolRegistry.Register(tools.NewShellSessionStartDefinition(shellSessionDeps))
	toolRegistry.Register(tools.NewShellSessionWriteDefinition(shellSessionDeps))
	toolRegistry.Register(tools.NewShellSessionCloseDefinition(shellSessionDeps))
	screenshotDeps := tools.ScreenshotDeps{
		Bridge:  dispatchBridge,
		Jobs:    jobStore,
		Cancels: watchCancels,
		Audit:   auditLogger,
		Online: func(clientID string) bool {
			_, ok := hub.Connection(clientID)
			return ok
		},
	}
	toolRegistry.Register(tools.NewScreenshotCaptureDefinition(screenshotDeps))
	toolRegistry.Register(tools.NewScreenshotWatchDefinition(screenshotDeps))
	inputDeps := tools.InputDeps{
		Bridge:             dispatchBridge,
		Audit:              auditLogger,
		ElicitationTimeout: cfg.elicitationTimeout,
	}
	toolRegistry.Register(tools.NewInputKeyDefinition(inputDeps))
	toolRegistry.Register(tools.NewInputMouseClickDefinition(inputDeps))
	toolRegistry.Register(tools.NewInputMouseMoveDefinition(inputDeps))
	toolRegistry.Register(tools.NewInputTypeDefinition(inputDeps))
	mcpHandler := transport.NewHandler(store, toolRegistry)
	mcpHandler.Resources = resourceRegistry
	mcpHandler.Prompts = prompts.NewRegistry(registry, dispatchBridge)
	mcpHandler.Completions = completions.NewRegistry(registry, dispatchBridge)
	mcpHandler.RateLimit = transport.NewRateLimiter(cfg.rateLimitSession, cfg.rateLimitTools, cfg.maxConcurrentDispatches)
	var mcpHTTPHandler http.Handler = mcpHandler
	mcpHTTPHandler = transport.OriginMiddleware(cfg.allowedOrigins, mcpHTTPHandler)
	mcpHTTPHandler = transport.AuthMiddleware(cfg.authToken, mcpHTTPHandler)

	mcpMux := http.NewServeMux()
	mcpMux.Handle("/agent/ws", hub)
	mcpMux.Handle("/mcp", mcpHTTPHandler)
	mcpMux.HandleFunc("/health", healthHandler(hub))

	mcpServer := &http.Server{Addr: cfg.mcpBindAddr, Handler: mcpMux}
	adminServer := &http.Server{Addr: cfg.adminBindAddr, Handler: adminAPI.Handler()}

	serveErr := make(chan error, 2)
	go func() {
		log.Printf("rc-mcp-server: MCP + agent WS listening on %s", cfg.mcpBindAddr)
		if err := mcpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	go func() {
		log.Printf("rc-mcp-server: admin API listening on %s (loopback only)", cfg.adminBindAddr)
		if err := adminServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Println("rc-mcp-server: shutdown signal received, draining")
	}

	timeout := cfg.shutdownTimeout
	if timeout <= 0 {
		timeout = shutdownTimeout
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Notify connected agents before tearing down the listeners so their
	// "close" envelope has a chance to reach them.
	hub.Shutdown("server_shutdown")

	var shutdownErr error
	if err := mcpServer.Shutdown(shutdownCtx); err != nil {
		shutdownErr = err
	}
	if err := adminServer.Shutdown(shutdownCtx); err != nil && shutdownErr == nil {
		shutdownErr = err
	}

	log.Println("rc-mcp-server: audit log flushing (deferred close)")

	if shutdownErr != nil {
		log.Printf("rc-mcp-server: shutdown did not complete cleanly: %v", shutdownErr)
		return shutdownErr
	}
	log.Println("rc-mcp-server: shutdown complete")
	return nil
}

type healthResponse struct {
	Status       string `json:"status"`
	AgentsOnline int    `json:"agents_online"`
}

func healthHandler(hub *agent.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status:       "ok",
			AgentsOnline: hub.AgentsOnline(),
		})
	}
}

type config struct {
	mcpBindAddr             string
	adminBindAddr           string
	deviceRegistryPath      string
	pairingCodeTTL          time.Duration
	reconnectGracePeriod    time.Duration
	authToken               string
	allowedOrigins          []string
	sseReplayBufferSize     int
	sessionIdleTimeout      time.Duration
	auditLogPath            string
	rateLimitSession        int
	rateLimitTools          int
	maxConcurrentDispatches int
	shellSkipConfirm        bool
	processSkipConfirm      bool
	fsSkipConfirm           bool
	shellConfirmEveryWrite  bool
	shellAllowlist          []string
	shellDenylist           []string
	auditFullArgs           bool
	globalFSAllowedRoots    []string
	sessionStore            string
	redisAddr               string
	replicaID               string
	agentLatestVersion      string
	maxShellSessions        int
	elicitationTimeout      time.Duration
	// shutdownTimeout overrides the default 30s graceful shutdown budget;
	// zero means "use the default". Only ever set explicitly in tests.
	shutdownTimeout time.Duration
}

func loadConfig() config {
	return config{
		mcpBindAddr:             envOr("MCP_BIND_ADDR", "0.0.0.0:8080"),
		adminBindAddr:           envOr("ADMIN_BIND_ADDR", "127.0.0.1:9090"),
		deviceRegistryPath:      envOr("DEVICE_REGISTRY_PATH", "/var/lib/rc-mcp/devices.json"),
		pairingCodeTTL:          envDurationOr("PAIRING_CODE_TTL", 5*time.Minute),
		reconnectGracePeriod:    envDurationOr("AGENT_RECONNECT_GRACE_PERIOD", agent.DefaultReconnectGracePeriod),
		authToken:               os.Getenv("AUTH_TOKEN"),
		allowedOrigins:          envCSVOr("MCP_ALLOWED_ORIGINS", nil),
		sseReplayBufferSize:     envIntOr("SSE_REPLAY_BUFFER_SIZE", session.DefaultReplayBufferSize),
		sessionIdleTimeout:      envDurationOr("MCP_SESSION_IDLE_TIMEOUT", session.DefaultIdleTimeout),
		auditLogPath:            envOr("RC_AUDIT_LOG_PATH", "/var/log/rc-mcp/audit.log"),
		rateLimitSession:        envIntOr("RC_RATE_LIMIT_SESSION", transport.DefaultRateLimitSession),
		rateLimitTools:          envIntOr("RC_RATE_LIMIT_TOOLS", transport.DefaultRateLimitTools),
		maxConcurrentDispatches: envIntOr("RC_MAX_CONCURRENT_DISPATCHES", transport.DefaultMaxConcurrentDispatches),
		shellSkipConfirm:        envBoolOr("RC_SHELL_SKIP_CONFIRM", false),
		processSkipConfirm:      envBoolOr("RC_PROCESS_SKIP_CONFIRM", false),
		fsSkipConfirm:           envBoolOr("RC_FS_SKIP_CONFIRM", false),
		shellConfirmEveryWrite:  envBoolOr("RC_SHELL_CONFIRM_EVERY_WRITE", false),
		shellAllowlist:          shellpolicy.ParsePatterns(os.Getenv("RC_SHELL_ALLOWLIST")),
		shellDenylist:           shellpolicy.ParsePatterns(os.Getenv("RC_SHELL_DENYLIST")),
		auditFullArgs:           envBoolOr("RC_AUDIT_FULL_ARGS", false),
		globalFSAllowedRoots:    fsroot.ParseRoots(os.Getenv("RC_GLOBAL_FS_ALLOWED_ROOTS")),
		sessionStore:            envOr("MCP_SESSION_STORE", sessionStoreMemory),
		redisAddr:               envOr("REDIS_ADDR", "localhost:6379"),
		replicaID:               envOr("REPLICA_ID", defaultReplicaID()),
		agentLatestVersion:      os.Getenv("AGENT_LATEST_VERSION"),
		maxShellSessions:        envIntOr("RC_MAX_SHELL_SESSIONS", tools.DefaultMaxShellSessions),
		elicitationTimeout:      envDurationOr("RC_ELICITATION_TIMEOUT", transport.DefaultElicitationTimeout),
	}
}

func envCSVOr(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("rc-mcp-server: invalid %s=%q, using default %d: %v", key, v, def, err)
		return def
	}
	return n
}

func envBoolOr(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("rc-mcp-server: invalid %s=%q, using default %v: %v", key, v, def, err)
		return def
	}
	return b
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// defaultReplicaID generates a fallback REPLICA_ID (Section 10, Section
// 19: "multi-replica") when the operator hasn't set one explicitly: the
// hostname (for readability in logs) plus a short random suffix (so two
// replicas that happen to share a hostname, e.g. identical container
// images with HOSTNAME unset, still get distinct IDs). Only used in
// multi-replica mode (MCP_SESSION_STORE=redis) -- a single-instance
// deployment never looks at it.
func defaultReplicaID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "replica"
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return host
	}
	return fmt.Sprintf("%s-%s", host, hex.EncodeToString(suffix))
}

func envDurationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("rc-mcp-server: invalid %s=%q, using default %s: %v", key, v, def, err)
		return def
	}
	return d
}
