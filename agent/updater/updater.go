// Package updater implements the agent's opt-in auto-update mechanism
// (docs/specs/backend.md Section 19, phase-3-post-mvp Risks "unsigned
// binary replacement"): on connect, if the server advertises a different
// version than the running binary and AGENT_AUTO_UPDATE=true, download the
// new binary from the release pipeline's endpoint (docs/operations/
// agent-releases.md), verify it against its published checksum, and only
// on success replace the running binary and restart.
//
// Checksum verification is mandatory and cannot be bypassed: a binary that
// fails verification is never installed, and the update is aborted (the
// agent keeps running its current, already-verified binary) rather than
// crashing the process over a failed update.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// ErrChecksumMismatch is returned when a downloaded binary's SHA-256 does
// not match its published checksum. The agent must refuse to install such
// a binary.
var ErrChecksumMismatch = errors.New("updater: downloaded binary failed checksum verification")

// downloadTimeout bounds each HTTP fetch (binary + checksum), so a stalled
// or malicious server can't hang the agent indefinitely mid-update.
const downloadTimeout = 60 * time.Second

// ShouldUpdate reports whether latestVersion (as advertised by the server
// in hello_ack) warrants an update over the currently running version.
// Empty or "dev" values never trigger an update -- "dev" marks a local,
// untagged build with no corresponding release to fetch, and an empty
// latestVersion means the server has nothing configured to advertise.
func ShouldUpdate(currentVersion, latestVersion string) bool {
	if latestVersion == "" || latestVersion == "dev" || currentVersion == "dev" {
		return false
	}
	return currentVersion != latestVersion
}

// Config configures one Update run.
type Config struct {
	// BaseURL is the release download base, e.g.
	// "https://github.com/champi-ai/rc-mcp/releases/download" (see
	// docs/operations/agent-releases.md for the full URL pattern).
	BaseURL string
	// GOOS/GOARCH select which published binary to fetch. Defaults to the
	// running binary's own runtime.GOOS/GOARCH when empty.
	GOOS, GOARCH string
	// HTTPClient is used for both downloads. Defaults to
	// &http.Client{Timeout: downloadTimeout} when nil.
	HTTPClient *http.Client
	// SystemdUnit is the unit name to restart after a successful replace
	// (AGENT_SYSTEMD_UNIT, default "rc-mcp-agent"). If restarting via
	// systemd fails (e.g. not running under systemd -- a dev/test
	// environment), Update falls back to re-executing the new binary
	// in-place via syscall.Exec so the update still takes effect.
	SystemdUnit string
}

// releaseURLs returns the binary and checksum URLs for version under
// cfg.BaseURL, following the pattern documented in
// docs/operations/agent-releases.md.
func (cfg Config) releaseURLs(version string) (binaryURL, checksumURL string) {
	goos, goarch := cfg.GOOS, cfg.GOARCH
	if goos == "" {
		goos = "linux"
	}
	if goarch == "" {
		goarch = defaultArch()
	}
	name := fmt.Sprintf("rc-mcp-agent-%s-%s", goos, goarch)
	base := strings.TrimSuffix(cfg.BaseURL, "/") + "/agent-v" + version + "/" + name
	return base, base + ".sha256"
}

// Update downloads version, verifies it, replaces the binary at
// execPath, and restarts (systemd, or a self-exec fallback). It returns an
// error and leaves execPath untouched if any step before the replace
// fails; a failure during or after the replace either restarts the
// process anyway (best effort, since the new binary is already verified
// and in place) or is reported for the caller to log -- the caller should
// treat any Update error as "the update did not happen this cycle, keep
// running the current binary" rather than fatal.
func Update(ctx context.Context, cfg Config, version, execPath string) error {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: downloadTimeout}
	}
	if cfg.SystemdUnit == "" {
		cfg.SystemdUnit = "rc-mcp-agent"
	}

	binaryURL, checksumURL := cfg.releaseURLs(version)

	checksumBody, err := fetch(ctx, cfg.HTTPClient, checksumURL)
	if err != nil {
		return fmt.Errorf("updater: fetch checksum: %w", err)
	}
	wantSum, err := parseChecksumFile(checksumBody)
	if err != nil {
		return fmt.Errorf("updater: parse checksum: %w", err)
	}

	binaryData, err := fetch(ctx, cfg.HTTPClient, binaryURL)
	if err != nil {
		return fmt.Errorf("updater: fetch binary: %w", err)
	}

	gotSum := sha256.Sum256(binaryData)
	if hex.EncodeToString(gotSum[:]) != wantSum {
		return ErrChecksumMismatch
	}

	if err := replaceSelfFn(binaryData, execPath); err != nil {
		return fmt.Errorf("updater: replace binary: %w", err)
	}

	restartFn(cfg.SystemdUnit, execPath)
	return nil
}

// replaceSelfFn and restartFn are package vars (rather than direct calls)
// so tests can substitute fakes -- restart's real implementation replaces
// the calling process image via syscall.Exec, which a test must never
// trigger for real.
var (
	replaceSelfFn = replaceSelf
	restartFn     = restart
)

func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// parseChecksumFile extracts the hex digest from a `sha256sum`-format
// file: "<hex digest>  <filename>", possibly with trailing whitespace/
// newline. Only the first line is considered.
func parseChecksumFile(data []byte) (string, error) {
	line := strings.TrimSpace(string(data))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("malformed checksum file content: %q", line)
	}
	return strings.ToLower(fields[0]), nil
}

// replaceSelf writes data to a temp file next to execPath, makes it
// executable, and atomically renames it over execPath. Writing to a
// sibling temp file and renaming (rather than truncating execPath
// in-place) means a crash mid-write never leaves a half-written,
// unexecutable binary at execPath.
func replaceSelf(data []byte, execPath string) error {
	dir := filepath.Dir(execPath)
	tmp, err := os.CreateTemp(dir, ".rc-mcp-agent-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpPath, execPath)
}

// restart attempts `systemctl restart <unit>` first; if that fails (not
// running under systemd, insufficient privilege, etc.), it falls back to
// re-executing the newly-installed binary in-place via syscall.Exec so
// the update still takes effect without requiring a process manager.
// Errors are logged-worthy but there is no meaningful error to return: by
// this point the new binary is already verified and installed on disk, so
// even a fully failed restart attempt means the *next* natural process
// restart (a crash, a reboot, a manual restart) picks up the new version.
func restart(unit, execPath string) {
	if systemctlRestartFn(unit) == nil {
		return
	}
	// syscall.Exec replaces the current process image in place, keeping
	// the same PID -- the closest non-systemd equivalent to "restart".
	_ = execFn(execPath, os.Args, os.Environ())
}

// systemctlRestartFn and execFn isolate the two real side-effecting calls
// restart makes, so tests can verify the fallback path fires on a
// systemctl failure without ever invoking a real syscall.Exec (which
// would replace the test binary's own process image).
var (
	systemctlRestartFn = func(unit string) error { return exec.Command("systemctl", "restart", unit).Run() }
	execFn             = syscall.Exec
)

func defaultArch() string {
	// Mirrors the release pipeline's published architectures
	// (docs/operations/agent-releases.md): amd64 and arm64.
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}
