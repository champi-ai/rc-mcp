// Command rc-mcp-agent is the desktop agent entry point: it loads (or
// obtains via first-run pairing) a persistent device token, then runs the
// dial/connect/pair/reconnect lifecycle against rc-mcp-server. See
// docs/specs/backend.md Section 2.1, Section 12.2, and Section 15.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/CloudKeter/rc-mcp/agent/client"
	"github.com/CloudKeter/rc-mcp/agent/updater"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
)

// version is stamped at build time via -ldflags "-X main.version=...", set
// by the release pipeline (Section 19, "Agent binary release pipeline") to
// the tagged release version. "dev" for a local/untagged build.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("rc-mcp-agent: version %s", version)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("rc-mcp-agent: %v", err)
	}

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("rc-mcp-agent: %v", err)
	}
}

func run(ctx context.Context, cfg config) error {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown-host"
	}

	c := client.NewClient(cfg.serverURL)

	token, ok, err := client.LoadToken(cfg.tokenPath)
	if err != nil {
		return err
	}
	if !ok {
		log.Println("rc-mcp-agent: no device token found, starting first-run pairing")
		token, err = pairUntilApproved(ctx, c, hostname)
		if err != nil {
			return err
		}
		if err := client.SaveToken(cfg.tokenPath, token); err != nil {
			return err
		}
		log.Printf("rc-mcp-agent: device token saved to %s", cfg.tokenPath)
	} else {
		log.Println("rc-mcp-agent: found persisted device token, skipping pairing")
	}

	return connectionLifecycle(ctx, c, token, hostname, cfg)
}

// pairUntilApproved runs the pairing flow, transparently restarting it if
// the pairing code expires before an operator approves it.
func pairUntilApproved(ctx context.Context, c *client.Client, hostname string) (string, error) {
	for {
		res, err := c.Pair(ctx, hostname, os.Stdout)
		if errors.Is(err, client.ErrPairingExpired) {
			continue
		}
		if err != nil {
			return "", err
		}
		return res.DeviceToken, nil
	}
}

// connectionLifecycle connects (with reconnect backoff and re-auth via the
// persisted token) and dispatches messages until ctx is cancelled. The
// dispatcher (with its shell sessions) and the outbox survive individual
// connections, so a brief disconnect keeps local state alive: on a
// reconnect within the server's grace period (hello_ack resume:true) the
// buffered output flushes and in-flight work streams on; otherwise the
// orphaned state is torn down (Section 2.1).
func connectionLifecycle(ctx context.Context, c *client.Client, token, hostname string, cfg config) error {
	dispatcher := client.NewDispatcher()
	outbox := client.NewOutbox()

	// abandon tears down all state held across a disconnect: in-flight
	// dispatch loops, local shell sessions, and buffered output.
	abandon := func() {
		dispatcher.CancelAll()
		dispatcher.CloseAllShellSessions()
		outbox.Drop()
	}

	var graceTimer *time.Timer
	stopGrace := func() {
		if graceTimer != nil {
			graceTimer.Stop()
			graceTimer = nil
		}
	}
	defer stopGrace()

	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		conn, ack, err := c.Connect(ctx, token, hostname, cfg.capabilities)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			attempt++
			delay := client.NextBackoff(attempt)
			log.Printf("rc-mcp-agent: connect failed (attempt %d): %v; retrying in %s", attempt, err, delay)
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return nil
			}
		}
		attempt = 0
		stopGrace()
		log.Printf("rc-mcp-agent: connected as device %s (resume=%v)", ack.DeviceID, ack.Resume)

		maybeAutoUpdate(ctx, cfg, ack.LatestAgentVersion)

		if ack.Resume {
			outbox.Attach(conn, true)
		} else {
			// The server is not holding our old dispatch state; anything
			// local from before this connection is orphaned.
			abandon()
			outbox.Attach(conn, false)
		}

		runConnection(ctx, conn, dispatcher, outbox)
		conn.Close()
		outbox.Detach()

		if ctx.Err() != nil {
			return nil
		}
		// Hold local state for the grace period; if the server doesn't
		// take us back with resume:true in time, clean it all up.
		graceTimer = time.AfterFunc(cfg.reconnectGrace, func() {
			log.Printf("rc-mcp-agent: reconnect grace period (%s) expired; cleaning up orphaned sessions", cfg.reconnectGrace)
			abandon()
		})
		log.Println("rc-mcp-agent: connection lost, reconnecting")
	}
}

// maybeAutoUpdate checks latestVersion (from hello_ack) against the
// running version and, if AGENT_AUTO_UPDATE is enabled and they differ,
// downloads, verifies, and installs the new binary, then restarts
// (Section 19). A failed update is logged and otherwise ignored: the
// agent keeps running its current, already-verified binary and will try
// again on the next connect.
//
// If the update succeeds via the systemd restart path, this process is
// about to be killed by systemd and never returns from here in practice;
// if it falls back to the self-exec path, this call never returns at all
// (the process image is replaced in place). Either way, the caller
// proceeding to runConnection afterward is only reachable when no update
// was needed or the update attempt failed.
func maybeAutoUpdate(ctx context.Context, cfg config, latestVersion string) {
	if !cfg.autoUpdate || !updater.ShouldUpdate(version, latestVersion) {
		return
	}
	log.Printf("rc-mcp-agent: update available (%s -> %s), downloading", version, latestVersion)

	execPath, err := os.Executable()
	if err != nil {
		log.Printf("rc-mcp-agent: auto-update: cannot determine own executable path: %v", err)
		return
	}

	updateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	uCfg := updater.Config{BaseURL: cfg.updateBaseURL, SystemdUnit: cfg.systemdUnit}
	if err := updater.Update(updateCtx, uCfg, latestVersion, execPath); err != nil {
		log.Printf("rc-mcp-agent: auto-update to %s failed, continuing on %s: %v", latestVersion, version, err)
		return
	}
	log.Printf("rc-mcp-agent: updated to %s, restarting", latestVersion)
}

// runConnection reads envelopes until the connection dies, ctx is
// cancelled, or a read error occurs. Each "dispatch" is routed to the
// dispatcher and run in its own goroutine (so a long-running shell_exec
// doesn't block reading a subsequent "cancel" for it); "cancel" messages
// are routed to the same dispatcher to interrupt any in-flight dispatch.
func runConnection(ctx context.Context, conn *client.Connection, dispatcher *client.Dispatcher, outbox *client.Outbox) {
	envCh := make(chan protocol.Envelope)
	readErrCh := make(chan error, 1)
	go func() {
		for {
			env, err := conn.ReadEnvelope()
			if err != nil {
				readErrCh <- err
				return
			}
			envCh <- env
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Dead():
			return
		case <-readErrCh:
			return
		case env := <-envCh:
			switch env.Type {
			case protocol.MsgDispatch:
				// Dispatches run detached from this connection: if it
				// drops mid-dispatch, the work continues and its output
				// buffers in the outbox for a resume (Section 2.1).
				go func(env protocol.Envelope) {
					result := dispatcher.HandleDispatch(ctx, env, outbox.SendBinary)
					_ = outbox.SendEnvelope(result)
				}(env)
			case protocol.MsgCancel:
				dispatcher.HandleCancel(env)
			default:
				// hello_ack/pair_*/ping/pong/close: not dispatch-related.
			}
		}
	}
}

type config struct {
	serverURL      string
	tokenPath      string
	capabilities   []string
	reconnectGrace time.Duration
	// autoUpdate, updateBaseURL, and systemdUnit configure the opt-in
	// auto-update mechanism (AGENT_AUTO_UPDATE, Section 19). autoUpdate
	// defaults to false: no update check or download occurs unless
	// explicitly enabled.
	autoUpdate    bool
	updateBaseURL string
	systemdUnit   string
}

func loadConfig() (config, error) {
	serverURL := os.Getenv("AGENT_SERVER_URL")
	if serverURL == "" {
		return config{}, errors.New("AGENT_SERVER_URL is required")
	}

	tokenPath := os.Getenv("AGENT_TOKEN_PATH")
	if tokenPath == "" {
		def, err := client.DefaultTokenPath()
		if err != nil {
			return config{}, err
		}
		tokenPath = def
	}

	capsRaw := os.Getenv("AGENT_CAPABILITIES")
	if capsRaw == "" {
		capsRaw = "shell,screenshot,filesystem,process,sysinfo"
	}
	var caps []string
	for _, c := range strings.Split(capsRaw, ",") {
		if c = strings.TrimSpace(c); c != "" {
			caps = append(caps, c)
		}
	}

	reconnectGrace := 60 * time.Second
	if v := os.Getenv("AGENT_RECONNECT_GRACE_PERIOD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			reconnectGrace = d
		}
	}

	autoUpdate := false
	if v := os.Getenv("AGENT_AUTO_UPDATE"); v != "" {
		autoUpdate, _ = strconv.ParseBool(v)
	}
	updateBaseURL := os.Getenv("AGENT_UPDATE_BASE_URL")
	if updateBaseURL == "" {
		updateBaseURL = "https://github.com/CloudKeter/rc-mcp/releases/download"
	}
	systemdUnit := os.Getenv("AGENT_SYSTEMD_UNIT")
	if systemdUnit == "" {
		systemdUnit = "rc-mcp-agent"
	}

	return config{
		serverURL:      serverURL,
		tokenPath:      tokenPath,
		capabilities:   caps,
		reconnectGrace: reconnectGrace,
		autoUpdate:     autoUpdate,
		updateBaseURL:  updateBaseURL,
		systemdUnit:    systemdUnit,
	}, nil
}
