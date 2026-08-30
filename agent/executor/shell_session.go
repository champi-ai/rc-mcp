// This file implements the PTY-backed interactive shell session executor
// (shell_session_start/write/close). See docs/specs/backend.md Section
// 3.1.2-3.1.4.
package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/champi-ai/rc-mcp/internal/mcp/types"
)

const (
	// DefaultShellSessionShell is used when neither ShellSessionStartInput.Shell
	// nor $SHELL is set.
	DefaultShellSessionShell = "/bin/bash"

	// ptyStreamInterval/ptyStreamBytes implement the "every 200ms or 4KB"
	// cadence from Section 3.1.3.
	ptyStreamInterval = 200 * time.Millisecond
	ptyStreamBytes    = 4096

	// DefaultShellSessionIdleTimeout is how long shell_session_write waits
	// for the PTY to go quiet before returning its accumulated output.
	DefaultShellSessionIdleTimeout = 2 * time.Second
	// DefaultShellSessionReadTimeout bounds shell_session_write overall,
	// even if the PTY never goes idle.
	DefaultShellSessionReadTimeout = 30 * time.Second
	// DefaultShellSessionCloseGrace is how long shell_session_close waits
	// after SIGTERM before escalating to SIGKILL.
	DefaultShellSessionCloseGrace = 5 * time.Second

	// DefaultMaxShellSessions is the default RC_MAX_SHELL_SESSIONS cap,
	// enforced by callers (typically per MCP session) via
	// ShellSessionManager.Count.
	DefaultMaxShellSessions = 5
)

// ErrShellSessionNotFound is returned by ShellSessionManager methods when
// the given shellSessionId is unknown (never existed, or already closed and
// reaped).
var ErrShellSessionNotFound = errors.New("executor: shell session not found")

// ShellSession is one PTY-backed interactive shell running on the agent.
type ShellSession struct {
	ID       string
	PID      int
	Shell    string
	ClientID string

	ptyFile *os.File
	cmd     *exec.Cmd
	doneCh  chan struct{}

	mu           sync.Mutex
	streamFn     StreamFunc
	outputBuf    bytes.Buffer
	lastActivity time.Time
	exited       bool
	exitCode     int
}

// ShellSessionManager tracks all PTY sessions currently open on this agent,
// keyed by shellSessionId.
type ShellSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*ShellSession
}

// NewShellSessionManager constructs an empty ShellSessionManager.
func NewShellSessionManager() *ShellSessionManager {
	return &ShellSessionManager{sessions: make(map[string]*ShellSession)}
}

// Count returns the number of currently open (not-yet-closed) sessions.
func (m *ShellSessionManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Get returns the session for id, or ErrShellSessionNotFound.
func (m *ShellSessionManager) Get(id string) (*ShellSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return nil, ErrShellSessionNotFound
	}
	return sess, nil
}

// Start allocates a PTY, spawns input.Shell (or $SHELL / DefaultShellSessionShell),
// and registers the resulting session under a freshly minted shellSessionId.
func (m *ShellSessionManager) Start(clientID string, input types.ShellSessionStartInput) (*ShellSession, error) {
	shell := DefaultShellSessionShell
	if input.Shell != nil && *input.Shell != "" {
		shell = *input.Shell
	} else if envShell := os.Getenv("SHELL"); envShell != "" {
		shell = envShell
	}

	cwd := ""
	if input.Cwd != nil && *input.Cwd != "" {
		cwd = *input.Cwd
	} else if home, err := os.UserHomeDir(); err == nil {
		cwd = home
	}

	rows, cols := 24, 80
	if input.Rows != nil && *input.Rows > 0 {
		rows = *input.Rows
	}
	if input.Cols != nil && *input.Cols > 0 {
		cols = *input.Cols
	}

	cmd := exec.Command(shell)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = os.Environ()
	for k, v := range input.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// pty.StartWithSize starts the process in its own session with the pty
	// slave as its controlling terminal, so cmd.Process.Pid also identifies
	// its process group -- killing -pid reaches any children it forks too
	// (the same process-tree lesson as shell_exec's SIGKILL handling).
	ptyFile, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("shell_session_start: failed to allocate pty: %w", err)
	}

	id, err := newShellSessionID()
	if err != nil {
		_ = ptyFile.Close()
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("shell_session_start: failed to mint session id: %w", err)
	}

	sess := &ShellSession{
		ID:           id,
		PID:          cmd.Process.Pid,
		Shell:        shell,
		ClientID:     clientID,
		ptyFile:      ptyFile,
		cmd:          cmd,
		doneCh:       make(chan struct{}),
		lastActivity: time.Now(),
	}

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	go sess.readLoop()

	return sess, nil
}

// Write sends input to sess's PTY. It returns the number of bytes written.
func (s *ShellSession) Write(input string) (int, error) {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return 0, fmt.Errorf("shell_session_write: session %s already exited", s.ID)
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()

	n, err := s.ptyFile.Write([]byte(input))
	if err != nil {
		return n, fmt.Errorf("shell_session_write: pty write failed: %w", err)
	}
	return n, nil
}

// StreamUntilIdleOrTimeout streams PTY output via onChunk (per the 200ms/4KB
// cadence) until the PTY has been quiet for idleTimeout, readTimeout
// elapses, or the shell process exits -- whichever comes first. It returns
// the output accumulated since the previous call (or session start),
// whether the process has exited, and its exit code if so.
func (s *ShellSession) StreamUntilIdleOrTimeout(ctx context.Context, onChunk StreamFunc, idleTimeout, readTimeout time.Duration) (output string, exited bool, exitCode int) {
	if idleTimeout <= 0 {
		idleTimeout = DefaultShellSessionIdleTimeout
	}
	if readTimeout <= 0 {
		readTimeout = DefaultShellSessionReadTimeout
	}

	s.mu.Lock()
	s.streamFn = onChunk
	s.lastActivity = time.Now() // restart idle counting for this call
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.streamFn = nil
		s.mu.Unlock()
	}()

	deadline := time.Now().Add(readTimeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The session itself stays alive (it is session-scoped, not
			// request-scoped, per Section 9's dispatch pattern (b) note for
			// shell_session_write); only this write's streaming wait ends.
			return s.drainOutput(), s.hasExited(), s.currentExitCode()
		case <-s.doneCh:
			return s.drainOutput(), true, s.currentExitCode()
		case <-ticker.C:
			s.mu.Lock()
			idle := time.Since(s.lastActivity) >= idleTimeout
			s.mu.Unlock()
			if idle || time.Now().After(deadline) {
				return s.drainOutput(), s.hasExited(), s.currentExitCode()
			}
		}
	}
}

func (s *ShellSession) drainOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.outputBuf.String()
	s.outputBuf.Reset()
	return out
}

func (s *ShellSession) hasExited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

func (s *ShellSession) currentExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
}

func (s *ShellSession) readLoop() {
	streamer := newChunkStreamerWithCadence(func(chunk []byte) {
		s.mu.Lock()
		fn := s.streamFn
		s.mu.Unlock()
		if fn != nil {
			fn(chunk)
		}
	}, ptyStreamInterval, ptyStreamBytes)

	buf := make([]byte, 4096)
	for {
		n, err := s.ptyFile.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.outputBuf.Write(chunk)
			s.lastActivity = time.Now()
			s.mu.Unlock()
			streamer.write(chunk)
		}
		if err != nil {
			streamer.flush()
			break
		}
	}

	exitCode := waitExitCode(s.cmd)

	s.mu.Lock()
	s.exited = true
	s.exitCode = exitCode
	s.mu.Unlock()
	close(s.doneCh)
}

// Close terminates sess: sends signal (default SIGTERM), waits up to grace
// for the process to exit, then escalates to SIGKILL. It kills the whole
// process group (see the process-tree kill note on Start) so children the
// shell forked don't outlive it and keep the pty open.
func (m *ShellSessionManager) Close(id string, signal string, grace time.Duration) (exitCode int, finalOutput string, err error) {
	sess, err := m.Get(id)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		_ = sess.ptyFile.Close()
	}()

	if grace <= 0 {
		grace = DefaultShellSessionCloseGrace
	}

	if sess.hasExited() {
		return sess.currentExitCode(), sess.drainOutput(), nil
	}

	sig := syscall.SIGTERM
	if signal == "SIGKILL" {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-sess.PID, sig)

	select {
	case <-sess.doneCh:
	case <-time.After(grace):
		_ = syscall.Kill(-sess.PID, syscall.SIGKILL)
		select {
		case <-sess.doneCh:
		case <-time.After(DefaultShellSessionCloseGrace):
			// Give up waiting; report what we know so far rather than
			// hanging the close call forever.
		}
	}

	return sess.currentExitCode(), sess.drainOutput(), nil
}

func waitExitCode(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		if cmd.ProcessState != nil {
			return cmd.ProcessState.ExitCode()
		}
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func newShellSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "shsess_" + hex.EncodeToString(b), nil
}

// CloseAll terminates every open session (SIGTERM with a short grace, then
// SIGKILL via Close's escalation) and releases their PTYs. Used when the
// agent's reconnect grace period lapses (Section 2.1).
func (m *ShellSessionManager) CloseAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_, _, _ = m.Close(id, "SIGKILL", 0)
	}
}
