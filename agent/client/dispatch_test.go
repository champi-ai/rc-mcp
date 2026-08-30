package client

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/champi-ai/rc-mcp/agent/executor"
	"github.com/champi-ai/rc-mcp/internal/mcp/types"
	"github.com/champi-ai/rc-mcp/internal/protocol"
)

func dispatchEnvelope(correlationID string, input map[string]any) protocol.Envelope {
	return protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:      "shell_exec",
			RequestID: "req-1",
			SessionID: "sess-1",
			Input:     input,
		},
	}
}

func TestDispatcher_ShellExec_Success(t *testing.T) {
	d := NewDispatcher()
	in := dispatchEnvelope("corr-1", map[string]any{"command": "echo hello"})

	out := d.HandleDispatch(context.Background(), in, nil)

	if out.Type != protocol.MsgResult {
		t.Fatalf("type = %q, want %q", out.Type, protocol.MsgResult)
	}
	if out.ID != in.ID {
		t.Fatalf("correlation ID = %q, want %q", out.ID, in.ID)
	}
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode result payload: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected isError=false, got error=%q", result.Error)
	}
}

func TestDispatcher_UnknownTool_ReturnsErrorResultNotCrash(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-2",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "nonexistent_tool", // not registered in any phase
			Input: map[string]any{},
		},
	}

	out := d.HandleDispatch(context.Background(), in, nil)

	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode result payload: %v", err)
	}
	if !result.IsError || result.Error != "unknown_tool" {
		t.Fatalf("result = %+v, want isError=true error=unknown_tool", result)
	}
}

func TestDispatcher_MalformedPayload_RejectedNotCrash(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type:    protocol.MsgDispatch,
		ID:      "corr-3",
		Ts:      time.Now().UTC(),
		Payload: "not-a-dispatch-payload",
	}

	out := d.HandleDispatch(context.Background(), in, nil)

	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode result payload: %v", err)
	}
	if !result.IsError || result.Error != "invalid_payload" {
		t.Fatalf("result = %+v, want isError=true error=invalid_payload", result)
	}
}

func TestDispatcher_MissingCommand_RejectedAgentSide(t *testing.T) {
	d := NewDispatcher()
	in := dispatchEnvelope("corr-4", map[string]any{}) // no "command" field

	out := d.HandleDispatch(context.Background(), in, nil)

	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode result payload: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Error, "command") {
		t.Fatalf("result = %+v, want isError=true mentioning missing command", result)
	}
}

func TestDispatcher_TimeoutOutOfRange_RejectedAgentSide(t *testing.T) {
	d := NewDispatcher()
	in := dispatchEnvelope("corr-5", map[string]any{"command": "echo hi", "timeout": 9999})

	out := d.HandleDispatch(context.Background(), in, nil)

	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode result payload: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Error, "timeout") {
		t.Fatalf("result = %+v, want isError=true mentioning timeout out of range", result)
	}
}

func TestDispatcher_Cancel_KillsInFlightAndUnblocks(t *testing.T) {
	d := NewDispatcher()
	in := dispatchEnvelope("corr-6", map[string]any{"command": "sleep 30"})

	resultCh := make(chan protocol.Envelope, 1)
	go func() {
		resultCh <- d.HandleDispatch(context.Background(), in, nil)
	}()

	// Give HandleDispatch a moment to register the in-flight cancel func.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		_, registered := d.inFlight["corr-6"]
		d.mu.Unlock()
		if registered {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	d.HandleCancel(protocol.Envelope{Type: protocol.MsgCancel, ID: "corr-6", Ts: time.Now().UTC()})

	select {
	case out := <-resultCh:
		result, err := decodePayload[protocol.ResultPayload](out.Payload)
		if err != nil {
			t.Fatalf("decode result payload: %v", err)
		}
		output, err := decodePayload[map[string]any](result.Output)
		if err != nil {
			t.Fatalf("decode output: %v", err)
		}
		if output["killed"] != true {
			t.Fatalf("output = %+v, want killed=true", output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not unblock the in-flight dispatch (sleep 30 should have been killed almost immediately)")
	}
}

func TestDispatcher_Cancel_UnknownCorrelationIDIsNoOp(t *testing.T) {
	d := NewDispatcher()
	// Should not panic even though nothing is registered.
	d.HandleCancel(protocol.Envelope{Type: protocol.MsgCancel, ID: "no-such-id", Ts: time.Now().UTC()})
}

func TestDispatcher_ShellSession_StartWriteClose_RoundTrip(t *testing.T) {
	d := NewDispatcher()

	startIn := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-start",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "shell_session_start",
			Input: map[string]any{"clientId": "device-1", "shell": "/bin/sh"},
		},
	}
	startOut := d.HandleDispatch(context.Background(), startIn, nil)
	startResult, err := decodePayload[protocol.ResultPayload](startOut.Payload)
	if err != nil {
		t.Fatalf("decode start result: %v", err)
	}
	if startResult.IsError {
		t.Fatalf("shell_session_start failed: %s", startResult.Error)
	}
	startOutput, err := decodePayload[map[string]any](startResult.Output)
	if err != nil {
		t.Fatalf("decode start output: %v", err)
	}
	shellSessionID, _ := startOutput["shellSessionId"].(string)
	if shellSessionID == "" {
		t.Fatal("expected non-empty shellSessionId")
	}

	writeIn := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-write",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "shell_session_write",
			Input: map[string]any{"shellSessionId": shellSessionID, "input": "echo hello\n"},
		},
	}
	writeOut := d.HandleDispatch(context.Background(), writeIn, func([]byte) error { return nil })
	writeResult, err := decodePayload[protocol.ResultPayload](writeOut.Payload)
	if err != nil {
		t.Fatalf("decode write result: %v", err)
	}
	if writeResult.IsError {
		t.Fatalf("shell_session_write failed: %s", writeResult.Error)
	}

	closeIn := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-close",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "shell_session_close",
			Input: map[string]any{"shellSessionId": shellSessionID},
		},
	}
	closeOut := d.HandleDispatch(context.Background(), closeIn, nil)
	closeResult, err := decodePayload[protocol.ResultPayload](closeOut.Payload)
	if err != nil {
		t.Fatalf("decode close result: %v", err)
	}
	if closeResult.IsError {
		t.Fatalf("shell_session_close failed: %s", closeResult.Error)
	}
}

func TestDispatcher_ShellSessionWrite_UnknownSession_ReturnsToolError(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-w2",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "shell_session_write",
			Input: map[string]any{"shellSessionId": "nope", "input": "echo hi\n"},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError=true for unknown shell session")
	}
}

func TestDispatcher_ShellSessionClose_UnknownSession_ReturnsToolError(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-c2",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "shell_session_close",
			Input: map[string]any{"shellSessionId": "nope"},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError=true for unknown shell session")
	}
}

func TestDispatcher_ScreenshotCapture_NoDisplay_ReturnsToolError(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-sc1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "screenshot_capture",
			Input: map[string]any{"clientId": "device-1"},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// CI has no X11 display available, so this should surface as a tool
	// error (isError=true) rather than crash.
	if !result.IsError {
		t.Fatal("expected isError=true when no display is available")
	}
}

func TestDispatcher_ScreenshotWatch_MissingClientID_RejectedAgentSide(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-sw1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "screenshot_watch",
			Input: map[string]any{},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Error, "clientId") {
		t.Fatalf("result = %+v, want isError=true mentioning missing clientId", result)
	}
}

func TestDispatcher_FSWriteReadDelete_RoundTrip(t *testing.T) {
	d := NewDispatcher()
	dir := t.TempDir()
	path := dir + "/sub/hello.txt"

	writeIn := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-fw1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "fs_write",
			Input: map[string]any{"clientId": "device-1", "path": path, "content": "hello world"},
		},
	}
	writeOut := d.HandleDispatch(context.Background(), writeIn, nil)
	writeResult, err := decodePayload[protocol.ResultPayload](writeOut.Payload)
	if err != nil {
		t.Fatalf("decode write result: %v", err)
	}
	if writeResult.IsError {
		t.Fatalf("fs_write failed: %s", writeResult.Error)
	}

	readIn := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-fr1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "fs_read",
			Input: map[string]any{"clientId": "device-1", "path": path},
		},
	}
	readOut := d.HandleDispatch(context.Background(), readIn, nil)
	readResult, err := decodePayload[protocol.ResultPayload](readOut.Payload)
	if err != nil {
		t.Fatalf("decode read result: %v", err)
	}
	if readResult.IsError {
		t.Fatalf("fs_read failed: %s", readResult.Error)
	}
	readOutput, err := decodePayload[map[string]any](readResult.Output)
	if err != nil {
		t.Fatalf("decode read output: %v", err)
	}
	if readOutput["content"] != "hello world" {
		t.Fatalf("content = %v, want %q", readOutput["content"], "hello world")
	}

	deleteIn := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-fd1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "fs_delete",
			Input: map[string]any{"clientId": "device-1", "path": path},
		},
	}
	deleteOut := d.HandleDispatch(context.Background(), deleteIn, nil)
	deleteResult, err := decodePayload[protocol.ResultPayload](deleteOut.Payload)
	if err != nil {
		t.Fatalf("decode delete result: %v", err)
	}
	if deleteResult.IsError {
		t.Fatalf("fs_delete failed: %s", deleteResult.Error)
	}
}

func TestDispatcher_FSRead_LargeFile_StreamsBinaryFrames(t *testing.T) {
	d := NewDispatcher()
	dir := t.TempDir()
	path := dir + "/big.txt"

	big := make([]byte, executor.FSChunkThreshold+1000)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var frames [][]byte
	sendBinary := func(b []byte) error {
		frames = append(frames, append([]byte(nil), b...))
		return nil
	}

	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "00000000-0000-4000-8000-0000000000f2", // full UUID-shaped ID: CorrelationPrefix needs >= 8 hex chars
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "fs_read",
			Input: map[string]any{"clientId": "device-1", "path": path},
		},
	}
	out := d.HandleDispatch(context.Background(), in, sendBinary)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.IsError {
		t.Fatalf("fs_read failed: %s", result.Error)
	}
	if len(frames) == 0 {
		t.Fatal("expected large file content to be streamed as binary frames")
	}
	output, err := decodePayload[map[string]any](result.Output)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["content"] != "" && output["content"] != nil {
		t.Fatalf("expected empty inline content for a streamed large file, got %v", output["content"])
	}
}

func TestDispatcher_FSList_ReturnsEntries(t *testing.T) {
	d := NewDispatcher()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/a.txt", []byte("a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-fl1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "fs_list",
			Input: map[string]any{"clientId": "device-1", "path": dir},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.IsError {
		t.Fatalf("fs_list failed: %s", result.Error)
	}
}

func TestDispatcher_FSStat_UnknownPath_ReturnsToolError(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-fst1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "fs_stat",
			Input: map[string]any{"clientId": "device-1", "path": "/nonexistent/path/for/testing"},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError=true for nonexistent path")
	}
}

func TestDispatcher_ProcessList_ReturnsSelf(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-pl1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "process_list",
			Input: map[string]any{"clientId": "device-1", "limit": 100000},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.IsError {
		t.Fatalf("process_list failed: %s", result.Error)
	}
}

func TestDispatcher_ProcessInfo_UnknownPID_ReturnsToolError(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-pi1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "process_info",
			Input: map[string]any{"clientId": "device-1", "pid": 1 << 30},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError=true for unknown pid")
	}
}

func TestDispatcher_ProcessSignal_RejectsSelfPID(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-ps1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "process_signal",
			Input: map[string]any{"clientId": "device-1", "pid": os.Getpid(), "signal": "SIGTERM"},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError=true when signaling the agent's own PID")
	}
}

func TestDispatcher_SysinfoGet_SectionSubset(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-si1",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "sysinfo_get",
			Input: map[string]any{"clientId": "device-1", "sections": []string{"cpu"}},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.IsError {
		t.Fatalf("sysinfo_get failed: %s", result.Error)
	}
	output, err := decodePayload[map[string]any](result.Output)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["cpu"] == nil {
		t.Fatal("expected cpu section to be populated")
	}
	if output["memory"] != nil {
		t.Fatal("expected memory section to be omitted when only cpu requested")
	}
}

func TestDispatcher_SysinfoGet_MissingClientID(t *testing.T) {
	d := NewDispatcher()
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-si2",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "sysinfo_get",
			Input: map[string]any{},
		},
	}
	out := d.HandleDispatch(context.Background(), in, nil)
	result, err := decodePayload[protocol.ResultPayload](out.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError=true when clientId missing")
	}
}

// Backward-compatible package-level helper still works: with no clientId in
// the input, fs_read's own validation rejects it as a tool error rather
// than crashing.
func TestHandleDispatch_PackageLevelHelper(t *testing.T) {
	in := protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   "corr-7",
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:  "fs_read",
			Input: map[string]any{},
		},
	}
	out := HandleDispatch(in)
	if out.ID != in.ID {
		t.Fatalf("correlation ID = %q, want %q", out.ID, in.ID)
	}
}

func TestDispatcher_CancelAll_StopsInFlight(t *testing.T) {
	d := NewDispatcher()
	done := make(chan protocol.Envelope, 1)
	go func() {
		done <- d.HandleDispatch(context.Background(),
			dispatchEnvelope("de0adbee-e29b-41d4-a716-446655440002", map[string]any{
				"clientId": "dev-1", "command": "sleep 60", "timeout": 60,
			}), nil)
	}()

	// Wait until the dispatch registers, then cancel everything.
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		n := len(d.inFlight)
		d.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatch never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.CancelAll()

	select {
	case env := <-done:
		if env.Type != protocol.MsgResult {
			t.Fatalf("expected result envelope, got %s", env.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CancelAll did not stop the in-flight dispatch")
	}
}

func TestDispatcher_CloseAllShellSessions(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.shellSessions.Start("dev-1", types.ShellSessionStartInput{ClientID: "dev-1"}); err != nil {
		t.Skipf("cannot start PTY session in this environment: %v", err)
	}
	if d.shellSessions.Count() != 1 {
		t.Fatalf("Count = %d", d.shellSessions.Count())
	}
	d.CloseAllShellSessions()
	if d.shellSessions.Count() != 0 {
		t.Fatalf("Count after CloseAll = %d, want 0", d.shellSessions.Count())
	}
}

func inputDispatchEnvelope(tool, correlationID string, input map[string]any) protocol.Envelope {
	return protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:      tool,
			RequestID: "req-1",
			SessionID: "sess-1",
			Input:     input,
		},
	}
}

func TestDispatcher_InputKey_MissingFields(t *testing.T) {
	d := NewDispatcher()
	cases := []map[string]any{
		{"key": "Return"},                // missing clientId
		{"clientId": "dev-1", "key": ""}, // empty key
	}
	for _, in := range cases {
		env := d.HandleDispatch(context.Background(), inputDispatchEnvelope("input_key", "de0adbee-e29b-41d4-a716-446655440020", in), nil)
		result, err := decodePayload[protocol.ResultPayload](env.Payload)
		if err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if !result.IsError {
			t.Errorf("input = %v: expected an error result", in)
		}
	}
}

func TestDispatcher_InputKey_XdotoolUnavailable_ReturnsErrorNotCrash(t *testing.T) {
	// xdotool is not installed in the test environment: this exercises
	// the real ErrInputUnavailable path end-to-end through the dispatcher
	// without needing to fake package-private executor state.
	d := NewDispatcher()
	env := d.HandleDispatch(context.Background(), inputDispatchEnvelope("input_key", "de0adbee-e29b-41d4-a716-446655440021", map[string]any{
		"clientId": "dev-1", "key": "Return",
	}), nil)
	result, err := decodePayload[protocol.ResultPayload](env.Payload)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result when xdotool is unavailable")
	}
}

func TestDispatcher_InputMouseClick_SendsAckFrameOnAttempt(t *testing.T) {
	d := NewDispatcher()
	var frames [][]byte
	sendBinary := func(data []byte) error {
		frames = append(frames, data)
		return nil
	}
	env := d.HandleDispatch(context.Background(), inputDispatchEnvelope("input_mouse_click", "de0adbee-e29b-41d4-a716-446655440022", map[string]any{
		"clientId": "dev-1", "x": 10, "y": 20,
	}), sendBinary)
	result, err := decodePayload[protocol.ResultPayload](env.Payload)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result when xdotool is unavailable")
	}
	if len(frames) != 1 {
		t.Fatalf("expected exactly one FrameInputAck frame, got %d", len(frames))
	}
	header := protocol.DecodeBinaryHeader(frames[0])
	if header.FrameType != protocol.FrameInputAck {
		t.Fatalf("frame type = %v, want FrameInputAck", header.FrameType)
	}
	if frames[0][protocol.BinaryHeaderSize] != 0 {
		t.Fatal("ack byte should be 0 (failure) since the action failed")
	}
}

func TestDispatcher_InputMouseClick_InvalidButton(t *testing.T) {
	d := NewDispatcher()
	env := d.HandleDispatch(context.Background(), inputDispatchEnvelope("input_mouse_click", "de0adbee-e29b-41d4-a716-446655440023", map[string]any{
		"clientId": "dev-1", "x": 0, "y": 0, "button": "quadruple-click",
	}), nil)
	result, err := decodePayload[protocol.ResultPayload](env.Payload)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result for an unrecognized button")
	}
}

func TestDispatcher_InputMouseMove_MissingClientID(t *testing.T) {
	d := NewDispatcher()
	env := d.HandleDispatch(context.Background(), inputDispatchEnvelope("input_mouse_move", "de0adbee-e29b-41d4-a716-446655440024", map[string]any{
		"x": 1, "y": 2,
	}), nil)
	result, err := decodePayload[protocol.ResultPayload](env.Payload)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result for missing clientId")
	}
}

func TestDispatcher_InputType_MissingClientID(t *testing.T) {
	d := NewDispatcher()
	env := d.HandleDispatch(context.Background(), inputDispatchEnvelope("input_type", "de0adbee-e29b-41d4-a716-446655440025", map[string]any{
		"text": "hello",
	}), nil)
	result, err := decodePayload[protocol.ResultPayload](env.Payload)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result for missing clientId")
	}
}
