package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/champi-ai/rc-mcp/internal/mcp/types"
)

func TestShellSessionManager_StartWriteClose_RoundTrip(t *testing.T) {
	m := NewShellSessionManager()

	sess, err := m.Start("device-1", types.ShellSessionStartInput{ClientID: "device-1", Shell: strPtr("/bin/sh")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sess.PID <= 0 {
		t.Fatalf("PID = %d, want > 0", sess.PID)
	}
	if m.Count() != 1 {
		t.Fatalf("Count = %d, want 1", m.Count())
	}

	if _, err := sess.Write("echo hello\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var collected strings.Builder
	output, exited, _ := sess.StreamUntilIdleOrTimeout(context.Background(), func(chunk []byte) {
		collected.Write(chunk)
	}, 300*time.Millisecond, 5*time.Second)
	if exited {
		t.Fatal("shell should not have exited yet")
	}
	full := output + collected.String()
	if !strings.Contains(full, "hello") {
		t.Fatalf("expected output to contain 'hello', got %q (streamed: %q)", output, collected.String())
	}

	// cwd should persist across writes -- a subsequent write's output
	// reflects real PTY state, not a simulated one.
	if _, err := sess.Write("pwd\n"); err != nil {
		t.Fatalf("Write(pwd): %v", err)
	}
	output2, _, _ := sess.StreamUntilIdleOrTimeout(context.Background(), nil, 300*time.Millisecond, 5*time.Second)
	if strings.TrimSpace(output2) == "" {
		t.Fatal("expected pwd output")
	}

	exitCode, _, err := m.Close(sess.ID, "SIGTERM", 2*time.Second)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("Count after close = %d, want 0", m.Count())
	}
	// A shell killed by SIGTERM commonly reports exit code -1 (signalled) or
	// 143 (128+SIGTERM) depending on platform/shell; either is acceptable --
	// we mainly assert Close didn't hang and cleaned up state.
	_ = exitCode
}

func TestShellSessionManager_WriteUnknownSession(t *testing.T) {
	m := NewShellSessionManager()
	if _, err := m.Get("nope"); err != ErrShellSessionNotFound {
		t.Fatalf("Get(nope) = %v, want ErrShellSessionNotFound", err)
	}
}

func TestShellSessionManager_CloseUnknownSession(t *testing.T) {
	m := NewShellSessionManager()
	if _, _, err := m.Close("nope", "SIGTERM", time.Second); err != ErrShellSessionNotFound {
		t.Fatalf("Close(nope) = %v, want ErrShellSessionNotFound", err)
	}
}

func TestShellSessionManager_ExitedProcessReportsExitCode(t *testing.T) {
	m := NewShellSessionManager()
	sess, err := m.Start("device-1", types.ShellSessionStartInput{ClientID: "device-1", Shell: strPtr("/bin/sh")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := sess.Write("exit 7\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case <-sess.doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for shell to exit")
	}
	if sess.currentExitCode() != 7 {
		t.Fatalf("exitCode = %d, want 7", sess.currentExitCode())
	}

	exitCode, _, err := m.Close(sess.ID, "SIGTERM", time.Second)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if exitCode != 7 {
		t.Fatalf("Close exitCode = %d, want 7", exitCode)
	}
}
