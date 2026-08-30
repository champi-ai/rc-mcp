package executor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/champi-ai/rc-mcp/internal/mcp/types"
	"github.com/champi-ai/rc-mcp/internal/protocol"
)

func intPtr(i int) *int       { return &i }
func strPtr(s string) *string { return &s }

func TestExec_EchoHello(t *testing.T) {
	result, err := Exec(context.Background(), types.ShellExecInput{Command: "echo hello"}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("stdout = %q, want %q", result.Stdout, "hello\n")
	}
	if result.ExitCode != 0 {
		t.Errorf("exitCode = %d, want 0", result.ExitCode)
	}
	if result.Killed {
		t.Error("killed = true, want false")
	}
}

func TestExec_CommandNotFound_ExitCode127(t *testing.T) {
	result, err := Exec(context.Background(), types.ShellExecInput{Command: "definitely-not-a-real-command-xyz"}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 127 {
		t.Errorf("exitCode = %d, want 127", result.ExitCode)
	}
	if result.Killed {
		t.Error("killed = true, want false")
	}
}

func TestExec_Timeout_Killed(t *testing.T) {
	result, err := Exec(context.Background(), types.ShellExecInput{
		Command: "echo partial; sleep 10",
		Timeout: intPtr(1),
	}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !result.Killed {
		t.Fatal("killed = false, want true")
	}
	if !strings.Contains(result.Stdout, "partial") {
		t.Errorf("stdout = %q, want to contain %q (partial output)", result.Stdout, "partial")
	}
}

func TestExec_TimeoutClampedToMax(t *testing.T) {
	// Not a behavioral timing test (that would need a 300s+ sleep); just
	// verifies MaxShellTimeout exists with the spec's value.
	if MaxShellTimeout != 300*time.Second {
		t.Fatalf("MaxShellTimeout = %v, want 300s", MaxShellTimeout)
	}
	if DefaultShellTimeout != 30*time.Second {
		t.Fatalf("DefaultShellTimeout = %v, want 30s", DefaultShellTimeout)
	}
}

func TestExec_ExitCode(t *testing.T) {
	result, err := Exec(context.Background(), types.ShellExecInput{Command: "exit 42"}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("exitCode = %d, want 42", result.ExitCode)
	}
}

func TestExec_Stderr(t *testing.T) {
	result, err := Exec(context.Background(), types.ShellExecInput{Command: "echo err-out 1>&2"}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stderr != "err-out\n" {
		t.Errorf("stderr = %q, want %q", result.Stderr, "err-out\n")
	}
}

func TestExec_Cwd(t *testing.T) {
	dir := t.TempDir()
	result, err := Exec(context.Background(), types.ShellExecInput{Command: "pwd", Cwd: strPtr(dir)}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != dir {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

func TestExec_Env(t *testing.T) {
	result, err := Exec(context.Background(), types.ShellExecInput{
		Command: "echo $MY_TEST_VAR",
		Env:     map[string]string{"MY_TEST_VAR": "hello-env"},
	}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "hello-env" {
		t.Errorf("stdout = %q, want %q", result.Stdout, "hello-env")
	}
}

func TestExec_Stdin(t *testing.T) {
	result, err := Exec(context.Background(), types.ShellExecInput{
		Command: "cat",
		Stdin:   strPtr("piped input\n"),
	}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "piped input\n" {
		t.Errorf("stdout = %q, want %q", result.Stdout, "piped input\n")
	}
}

func TestExec_StreamsChunks(t *testing.T) {
	var mu sync.Mutex
	var chunks [][]byte
	onChunk := func(c []byte) {
		mu.Lock()
		chunks = append(chunks, append([]byte(nil), c...))
		mu.Unlock()
	}

	result, err := Exec(context.Background(), types.ShellExecInput{
		Command: "printf 'a'; sleep 0.6; printf 'b'; sleep 0.6; printf 'c'",
	}, onChunk)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want >= 2 (the 0.6s sleep should force a flush before 'b' arrives)", len(chunks))
	}
	var total []byte
	for _, c := range chunks {
		total = append(total, c...)
	}
	if string(total) != result.Stdout {
		t.Errorf("concatenated chunks = %q, want to equal final stdout %q", total, result.Stdout)
	}
}

func TestExec_ChunksEncodeAsValidBinaryFrames(t *testing.T) {
	correlationID := "aabbccdd-e29b-41d4-a716-446655440000"
	prefix, err := protocol.CorrelationPrefix(correlationID)
	if err != nil {
		t.Fatalf("CorrelationPrefix: %v", err)
	}

	var seq uint32
	var frames [][]byte
	onChunk := func(c []byte) {
		buf := make([]byte, protocol.BinaryHeaderSize+len(c))
		protocol.EncodeBinaryHeader(buf, protocol.BinaryHeader{
			CorrelationPrefix: prefix,
			StreamSeq:         seq,
			FrameType:         protocol.FrameShellStdout,
		})
		copy(buf[protocol.BinaryHeaderSize:], c)
		seq++
		frames = append(frames, buf)
	}

	_, err = Exec(context.Background(), types.ShellExecInput{Command: "echo hello"}, onChunk)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("expected at least one streamed frame")
	}

	h := protocol.DecodeBinaryHeader(frames[0])
	if h.FrameType != protocol.FrameShellStdout {
		t.Errorf("frame type = %v, want FrameShellStdout", h.FrameType)
	}
	if h.CorrelationPrefix != prefix {
		t.Errorf("correlation prefix = %x, want %x", h.CorrelationPrefix, prefix)
	}
	if h.StreamSeq != 0 {
		t.Errorf("first frame seq = %d, want 0", h.StreamSeq)
	}
}

func TestExec_ContextCancelledIsKilled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	result, err := Exec(ctx, types.ShellExecInput{Command: "sleep 5"}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !result.Killed {
		t.Fatal("killed = false, want true")
	}
}
