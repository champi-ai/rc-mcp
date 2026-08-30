package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogger_WriteProducesValidJSONLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	err = logger.LogCall("sess-1", "dev-1", "shell_exec", map[string]any{"command": "ls -la"}, StatusOK, 42*time.Millisecond, "")
	if err != nil {
		t.Fatalf("LogCall: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}

	var entry Entry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	if entry.SessionID != "sess-1" || entry.ClientID != "dev-1" || entry.Tool != "shell_exec" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Status != StatusOK {
		t.Fatalf("status = %q, want ok", entry.Status)
	}
	if entry.DurationMs != 42 {
		t.Fatalf("durationMs = %d, want 42", entry.DurationMs)
	}
	if entry.Timestamp.IsZero() {
		t.Fatal("timestamp should be populated")
	}
	if entry.ArgsDigest == "" {
		t.Fatal("argsDigest should be populated")
	}
	if entry.ArgsHint == "" {
		t.Fatal("argsHint should be populated")
	}
}

func TestLogger_ArgsNeverLoggedRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	secret := "super-secret-payload-xyz"
	if err := logger.LogCall("sess-1", "dev-1", "shell_exec", map[string]any{"stdin": secret}, StatusOK, 0, ""); err != nil {
		t.Fatalf("LogCall: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("raw args leaked into audit log: %s", raw)
	}
}

func TestDigestArgs_Deterministic(t *testing.T) {
	d1 := DigestArgs(map[string]any{"a": 1})
	d2 := DigestArgs(map[string]any{"a": 1})
	if d1 != d2 {
		t.Fatalf("digest not deterministic: %s != %s", d1, d2)
	}
	if len(d1) != 64 {
		t.Fatalf("digest length = %d, want 64 (hex SHA-256)", len(d1))
	}
}

func TestHintFromMap_SortedAndTruncated(t *testing.T) {
	hint := HintFromMap(map[string]any{"b": 2, "a": 1}, 0)
	if hint != "a=1, b=2" {
		t.Fatalf("hint = %q, want %q", hint, "a=1, b=2")
	}

	long := HintFromMap(map[string]any{"x": strings.Repeat("y", 300)}, 20)
	if len(long) != 23 { // 20 + "..."
		t.Fatalf("truncated hint length = %d, want 23", len(long))
	}
}

func TestLogger_DetectsExternalRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	if err := logger.LogCall("sess-1", "dev-1", "tool1", nil, StatusOK, 0, ""); err != nil {
		t.Fatalf("LogCall: %v", err)
	}

	// Simulate logrotate: rename the current log away.
	rotated := filepath.Join(dir, "audit.log.1")
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if err := logger.LogCall("sess-1", "dev-1", "tool2", nil, StatusOK, 0, ""); err != nil {
		t.Fatalf("LogCall after rotation: %v", err)
	}

	rotatedLines := readLines(t, rotated)
	if len(rotatedLines) != 1 {
		t.Fatalf("rotated file has %d lines, want 1 (no entries lost)", len(rotatedLines))
	}

	newLines := readLines(t, path)
	if len(newLines) != 1 {
		t.Fatalf("new file has %d lines, want 1", len(newLines))
	}

	var e2 Entry
	if err := json.Unmarshal([]byte(newLines[0]), &e2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e2.Tool != "tool2" {
		t.Fatalf("new file's entry tool = %q, want tool2", e2.Tool)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestLogger_FullArgsOff_NoFullArgsField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	if err := logger.LogCall("sess-1", "dev-1", "shell_exec", map[string]any{"command": "ls -la"}, StatusOK, 0, ""); err != nil {
		t.Fatalf("LogCall: %v", err)
	}

	lines := readLines(t, path)
	var entry Entry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if entry.FullArgs != nil {
		t.Fatalf("fullArgs should be absent by default, got %s", entry.FullArgs)
	}
	if !strings.Contains(lines[0], `"argsDigest"`) {
		t.Fatal("digest-only mode should still be unchanged from before")
	}
	if strings.Contains(lines[0], `"fullArgs"`) {
		t.Fatal("the fullArgs key should not even appear in the JSON when disabled")
	}
}

func TestLogger_FullArgsOn_LogsCompleteArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()
	logger.FullArgs = true

	if err := logger.LogCall("sess-1", "dev-1", "shell_exec", map[string]any{"command": "ls -la", "clientId": "dev-1"}, StatusOK, 0, ""); err != nil {
		t.Fatalf("LogCall: %v", err)
	}

	lines := readLines(t, path)
	var entry Entry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if entry.FullArgs == nil {
		t.Fatal("fullArgs should be populated when FullArgs is enabled")
	}
	var full map[string]any
	if err := json.Unmarshal(entry.FullArgs, &full); err != nil {
		t.Fatalf("fullArgs is not valid JSON: %v", err)
	}
	if full["command"] != "ls -la" || full["clientId"] != "dev-1" {
		t.Fatalf("fullArgs = %+v, want the complete argument map", full)
	}
	// Digest/hint remain present alongside the full args.
	if entry.ArgsDigest == "" || entry.ArgsHint == "" {
		t.Fatal("argsDigest/argsHint should still be populated in full-args mode")
	}
}

func TestLogger_FullArgsOn_UnredactedEvenForSensitiveKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()
	logger.FullArgs = true

	secret := "super-secret-payload-xyz"
	if err := logger.LogCall("sess-1", "dev-1", "shell_session_write", map[string]any{"input": secret}, StatusOK, 0, ""); err != nil {
		t.Fatalf("LogCall: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Opt-in full-args logging is explicitly the forensic, unredacted
	// mode -- the operator has accepted that tradeoff (Section 12.7).
	if !strings.Contains(string(raw), secret) {
		t.Fatal("full-args mode should log the complete, unredacted argument value")
	}
}
