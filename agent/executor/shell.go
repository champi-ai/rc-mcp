// Package executor implements the desktop agent's tool executors: the
// code that actually spawns commands, allocates PTYs, reads the
// filesystem, etc. This file implements shell_exec. See
// docs/specs/backend.md Section 3.1.1 and Section 2.2.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/champi-ai/rc-mcp/internal/mcp/types"
)

const (
	// DefaultShellTimeout is used when ShellExecInput.Timeout is nil.
	DefaultShellTimeout = 30 * time.Second
	// MaxShellTimeout bounds ShellExecInput.Timeout regardless of what the
	// client requests.
	MaxShellTimeout = 300 * time.Second

	// streamMinInterval/streamMinBytes implement the "minimum interval
	// 500ms or 4KB, whichever comes first" streaming cadence from
	// Section 3.1.1.
	streamMinInterval = 500 * time.Millisecond
	streamMinBytes    = 4096
)

// StreamFunc is called with each combined stdout/stderr chunk as the
// command runs, per the streaming cadence above. It must not block for
// long, since it is called synchronously from the pipe-reading goroutines.
type StreamFunc func(chunk []byte)

// ShellExecResult is the outcome of one Exec call.
type ShellExecResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	Killed     bool
	DurationMs int64
}

// Exec spawns "/bin/sh -c <command>" per input, streaming combined
// stdout/stderr chunks to onChunk (if non-nil) as they arrive, and returns
// once the command exits, the timeout elapses (SIGKILL, Killed=true), or
// ctx is cancelled (also SIGKILL, Killed=true).
//
// A command that fails to be found by /bin/sh itself (e.g. "nosuchcmd")
// is not a Go-level start error: /bin/sh reports it on stderr and exits
// 127, which Exec surfaces as ExitCode 127, not an error.
func Exec(ctx context.Context, input types.ShellExecInput, onChunk StreamFunc) (ShellExecResult, error) {
	timeout := DefaultShellTimeout
	if input.Timeout != nil {
		timeout = time.Duration(*input.Timeout) * time.Second
	}
	if timeout <= 0 {
		timeout = DefaultShellTimeout
	}
	if timeout > MaxShellTimeout {
		timeout = MaxShellTimeout
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "/bin/sh", "-c", input.Command)

	// Run the shell in its own process group and kill the whole group on
	// cancel/timeout, not just the shell itself: /bin/sh may fork a child
	// for the actual command rather than exec-replacing itself, and an
	// orphaned child holding the stdout/stderr pipe open would otherwise
	// make cmd.Wait() block indefinitely (WaitDelay is a bounded safety
	// net for the same reason, in case the process group kill still
	// leaves something holding a pipe open).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	cwd := ""
	if input.Cwd != nil {
		cwd = *input.Cwd
	} else if home, err := os.UserHomeDir(); err == nil {
		cwd = home
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	cmd.Env = os.Environ()
	for k, v := range input.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	if input.Stdin != nil {
		cmd.Stdin = bytes.NewReader([]byte(*input.Stdin))
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	streamer := newChunkStreamer(onChunk)
	defer streamer.flush()

	if onChunk != nil {
		cmd.Stdout = newTeeWriter(&stdoutBuf, streamer)
		cmd.Stderr = newTeeWriter(&stderrBuf, streamer)
	} else {
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
	}

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)
	streamer.flush()

	result := ShellExecResult{
		Stdout:     stdoutBuf.String(),
		Stderr:     stderrBuf.String(),
		DurationMs: duration.Milliseconds(),
	}

	if execCtx.Err() != nil && errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		result.Killed = true
		result.ExitCode = -1
		return result, nil
	}
	// A parent-cancelled ctx (not our own timeout) also counts as killed.
	if ctx.Err() != nil {
		result.Killed = true
		result.ExitCode = -1
		return result, nil
	}

	var exitErr *exec.ExitError
	if err != nil {
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		// Command failed to start at all (e.g. /bin/sh missing) -- this is
		// a genuine execution error, not a shell-reported exit code.
		return result, fmt.Errorf("shell_exec: failed to start: %w", err)
	}

	result.ExitCode = cmd.ProcessState.ExitCode()
	return result, nil
}

// chunkStreamer batches writes and forwards them to onChunk no more often
// than minInterval, unless minBytes accumulates first.
type chunkStreamer struct {
	onChunk     StreamFunc
	minInterval time.Duration
	minBytes    int

	mu        sync.Mutex
	buf       bytes.Buffer
	lastFlush time.Time
}

// newChunkStreamer uses the shell_exec cadence (streamMinInterval/streamMinBytes).
func newChunkStreamer(onChunk StreamFunc) *chunkStreamer {
	return newChunkStreamerWithCadence(onChunk, streamMinInterval, streamMinBytes)
}

// newChunkStreamerWithCadence lets callers with a different streaming
// cadence (e.g. shell_session PTY output, Section 3.1.3: "every 200ms or
// 4KB") reuse the same batching logic.
func newChunkStreamerWithCadence(onChunk StreamFunc, minInterval time.Duration, minBytes int) *chunkStreamer {
	return &chunkStreamer{onChunk: onChunk, minInterval: minInterval, minBytes: minBytes, lastFlush: time.Now()}
}

func (s *chunkStreamer) write(p []byte) {
	if s.onChunk == nil {
		return
	}
	s.mu.Lock()
	s.buf.Write(p)
	ready := s.buf.Len() >= s.minBytes || time.Since(s.lastFlush) >= s.minInterval
	var chunk []byte
	if ready && s.buf.Len() > 0 {
		chunk = append([]byte(nil), s.buf.Bytes()...)
		s.buf.Reset()
		s.lastFlush = time.Now()
	}
	s.mu.Unlock()

	if chunk != nil {
		s.onChunk(chunk)
	}
}

func (s *chunkStreamer) flush() {
	if s.onChunk == nil {
		return
	}
	s.mu.Lock()
	var chunk []byte
	if s.buf.Len() > 0 {
		chunk = append([]byte(nil), s.buf.Bytes()...)
		s.buf.Reset()
	}
	s.mu.Unlock()

	if chunk != nil {
		s.onChunk(chunk)
	}
}

// teeWriter writes to an in-memory capture buffer and also streams through
// a chunkStreamer, without the two-writer races bytes.Buffer + io.MultiWriter
// would have across concurrent stdout/stderr goroutines (each teeWriter
// instance owns its own capture buffer; the shared streamer serializes via
// its own mutex).
type teeWriter struct {
	capture  *bytes.Buffer
	streamer *chunkStreamer
	mu       sync.Mutex
}

func newTeeWriter(capture *bytes.Buffer, streamer *chunkStreamer) *teeWriter {
	return &teeWriter{capture: capture, streamer: streamer}
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	n, err := t.capture.Write(p)
	t.mu.Unlock()
	t.streamer.write(p)
	return n, err
}
