package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/jobs"
	"github.com/champi-ai/rc-mcp/internal/mcp/types"
	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

func newScreenshotDeps(dispatch *fakeDispatcher, store jobs.JobStore) ScreenshotDeps {
	return ScreenshotDeps{
		Bridge:  dispatch,
		Jobs:    store,
		Cancels: NewWatchCancels(),
		Online:  func(string) bool { return true },
	}
}

func pngFrame(data []byte, seq uint32) *agent.BinaryFrame {
	return &agent.BinaryFrame{
		Header: protocol.BinaryHeader{StreamSeq: seq, FrameType: protocol.FrameScreenshotPNG},
		Data:   data,
	}
}

func callCapture(deps ScreenshotDeps, sess *session.Session, input map[string]any) (*transport.ToolCallResult, *transport.RPCError) {
	args, _ := json.Marshal(input)
	return deps.handleCapture(context.Background(), sess, transport.ToolCallMeta{RequestID: "req-1"}, args)
}

func callWatch(deps ScreenshotDeps, sess *session.Session, meta transport.ToolCallMeta, input map[string]any) (*transport.ToolCallResult, *transport.RPCError) {
	args, _ := json.Marshal(input)
	return deps.handleWatch(context.Background(), sess, meta, args)
}

// drainEvents collects notifications/progress params from the session's
// EventCh until want messages arrived or the timeout elapses.
func drainEvents(t *testing.T, sess *session.Session, want int, timeout time.Duration) []map[string]any {
	t.Helper()
	var out []map[string]any
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case ev := <-sess.EventCh:
			var msg struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
				t.Fatalf("bad event JSON: %v", err)
			}
			if msg.Method == "notifications/progress" {
				out = append(out, msg.Params)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %d progress events, got %d", want, len(out))
		}
	}
	return out
}

func waitForTerminal(t *testing.T, store jobs.JobStore, jobID string, timeout time.Duration) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := store.Get(jobID)
		if err == nil && job.Status.IsTerminal() {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach a terminal state within %v", jobID, timeout)
	return nil
}

func TestScreenshotCapture_MissingClientID(t *testing.T) {
	deps := newScreenshotDeps(&fakeDispatcher{}, jobs.NewMemoryStore(0, nil))
	sess := session.New(context.Background(), "sess-1", 10)

	_, rpcErr := callCapture(deps, sess, map[string]any{})
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("rpcErr = %+v, want code -32602", rpcErr)
	}
}

func TestScreenshotCapture_ReturnsImageContent(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		if tool != "screenshot_capture" {
			t.Errorf("tool = %q, want screenshot_capture", tool)
		}
		onProgress(nil, pngFrame(png, 0))
		return protocol.ResultPayload{
			Tool:   tool,
			Output: map[string]any{"width": 800, "height": 600, "mimeType": "image/png"},
		}, nil
	}}
	deps := newScreenshotDeps(dispatch, jobs.NewMemoryStore(0, nil))
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callCapture(deps, sess, map[string]any{"clientId": "dev-1"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result.Content)
	}
	if len(result.Content) < 2 || result.Content[0].Type != "image" {
		t.Fatalf("content = %+v, want image content first", result.Content)
	}
	decoded, err := base64.StdEncoding.DecodeString(result.Content[0].Data)
	if err != nil || string(decoded) != string(png) {
		t.Fatalf("image data does not round-trip: %v", err)
	}
	if result.Content[0].MimeType != "image/png" {
		t.Fatalf("mimeType = %q, want image/png", result.Content[0].MimeType)
	}
	var meta types.ScreenshotCaptureOutput
	if err := json.Unmarshal(result.StructuredContent, &meta); err != nil {
		t.Fatalf("structuredContent: %v", err)
	}
	if meta.Width != 800 || meta.Height != 600 || meta.ClientID != "dev-1" {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestScreenshotCapture_NoFrame_IsToolError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{"width": 1, "height": 1}}, nil
	}}
	deps := newScreenshotDeps(dispatch, jobs.NewMemoryStore(0, nil))
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callCapture(deps, sess, map[string]any{"clientId": "dev-1"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatalf("want tool error when no binary frame arrives, got %+v", result.Content)
	}
}

func TestScreenshotCapture_DeviceOffline(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrDeviceOffline
	}}
	deps := newScreenshotDeps(dispatch, jobs.NewMemoryStore(0, nil))
	sess := session.New(context.Background(), "sess-1", 10)

	result, _ := callCapture(deps, sess, map[string]any{"clientId": "dev-1"})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "offline") {
		t.Fatalf("result = %+v, want offline tool error", result)
	}
}

func TestScreenshotWatch_InvalidParams(t *testing.T) {
	deps := newScreenshotDeps(&fakeDispatcher{}, jobs.NewMemoryStore(0, nil))
	sess := session.New(context.Background(), "sess-1", 10)

	for name, input := range map[string]map[string]any{
		"missing clientId": {},
		"interval too low": {"clientId": "dev-1", "intervalMs": 100},
		"maxFrames high":   {"clientId": "dev-1", "maxFrames": 500},
		"duration high":    {"clientId": "dev-1", "durationSecs": 9999},
	} {
		_, rpcErr := callWatch(deps, sess, transport.ToolCallMeta{RequestID: "req-1"}, input)
		if rpcErr == nil || rpcErr.Code != -32602 {
			t.Errorf("%s: rpcErr = %+v, want -32602", name, rpcErr)
		}
	}
}

func TestScreenshotWatch_OfflineDevice_NoJobCreated(t *testing.T) {
	store := jobs.NewMemoryStore(0, nil)
	deps := newScreenshotDeps(&fakeDispatcher{}, store)
	deps.Online = func(string) bool { return false }
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callWatch(deps, sess, transport.ToolCallMeta{RequestID: "req-1"}, map[string]any{"clientId": "dev-1"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatalf("want offline tool error, got %+v", result.Content)
	}
	if jobsList, _ := store.ListBySession("sess-1", 0); len(jobsList) != 0 {
		t.Fatalf("no job should be created for an offline device, got %d", len(jobsList))
	}
}

func TestScreenshotWatch_HappyPath_JobAndFrames(t *testing.T) {
	frameA := []byte("frame-a")
	frameB := []byte("frame-b")
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		onProgress(nil, pngFrame(frameA, 0))
		onProgress(nil, pngFrame(frameB, 1))
		return protocol.ResultPayload{
			Tool:   tool,
			Output: map[string]any{"jobId": correlationID, "framesCaptured": 2, "durationMs": 40, "stoppedReason": "maxFrames"},
		}, nil
	}}
	store := jobs.NewMemoryStore(0, nil)
	deps := newScreenshotDeps(dispatch, store)
	sess := session.New(context.Background(), "sess-1", 10)

	result, rpcErr := callWatch(deps, sess, transport.ToolCallMeta{RequestID: "req-1", ProgressToken: "tok-1"}, map[string]any{"clientId": "dev-1", "maxFrames": 2})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result.Content)
	}
	var ack types.ScreenshotWatchAck
	if err := json.Unmarshal(result.StructuredContent, &ack); err != nil || ack.JobID == "" || ack.ClientID != "dev-1" {
		t.Fatalf("ack = %+v (err %v)", ack, err)
	}

	// Two frame events plus the terminal event.
	events := drainEvents(t, sess, 3, 2*time.Second)
	if data, _ := events[0]["frame"].(map[string]any); data == nil {
		t.Fatalf("first event has no frame: %+v", events[0])
	} else if b64, _ := data["data"].(string); b64 != base64.StdEncoding.EncodeToString(frameA) {
		t.Fatalf("first frame data mismatch")
	}
	last := events[len(events)-1]
	if last["message"] != "completed" || last["result"] == nil {
		t.Fatalf("terminal event = %+v", last)
	}

	job := waitForTerminal(t, store, ack.JobID, 2*time.Second)
	if job.Status != jobs.JobStatusSucceeded {
		t.Fatalf("job status = %s, want succeeded", job.Status)
	}
	var outcome types.ScreenshotWatchOutcome
	if err := json.Unmarshal(job.Result, &outcome); err != nil {
		t.Fatalf("job result: %v", err)
	}
	if outcome.FramesCaptured != 2 || outcome.StoppedReason != "maxFrames" || outcome.JobID != ack.JobID {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestScreenshotWatch_Idempotency_ReturnsSameJob(t *testing.T) {
	block := make(chan struct{})
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		<-block
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{"framesCaptured": 0, "stoppedReason": "maxFrames"}}, nil
	}}
	store := jobs.NewMemoryStore(0, nil)
	deps := newScreenshotDeps(dispatch, store)
	sess := session.New(context.Background(), "sess-1", 10)
	meta := transport.ToolCallMeta{RequestID: "req-same"}

	res1, _ := callWatch(deps, sess, meta, map[string]any{"clientId": "dev-1"})
	res2, _ := callWatch(deps, sess, meta, map[string]any{"clientId": "dev-1"})
	close(block)

	var ack1, ack2 types.ScreenshotWatchAck
	_ = json.Unmarshal(res1.StructuredContent, &ack1)
	_ = json.Unmarshal(res2.StructuredContent, &ack2)
	if ack1.JobID == "" || ack1.JobID != ack2.JobID {
		t.Fatalf("retry should return the same jobId: %q vs %q", ack1.JobID, ack2.JobID)
	}
	if jobsList, _ := store.ListBySession("sess-1", 0); len(jobsList) != 1 {
		t.Fatalf("want exactly 1 job, got %d", len(jobsList))
	}
}

func TestScreenshotWatch_CancelViaSession_StopsAndKeepsPartial(t *testing.T) {
	frame := []byte("partial-frame")
	started := make(chan struct{})
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		onProgress(nil, pngFrame(frame, 0))
		close(started)
		<-ctx.Done()
		// Mirror the real bridge: on cancel the agent responds with a
		// terminal result carrying stoppedReason "cancelled".
		return protocol.ResultPayload{
			Tool:   tool,
			Output: map[string]any{"framesCaptured": 1, "stoppedReason": "cancelled"},
		}, nil
	}}
	store := jobs.NewMemoryStore(0, nil)
	deps := newScreenshotDeps(dispatch, store)
	sess := session.New(context.Background(), "sess-1", 10)
	meta := transport.ToolCallMeta{RequestID: "req-cancel", ProgressToken: "tok-1"}

	result, rpcErr := callWatch(deps, sess, meta, map[string]any{"clientId": "dev-1"})
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	var ack types.ScreenshotWatchAck
	_ = json.Unmarshal(result.StructuredContent, &ack)

	<-started
	// Simulate the transport's UnregisterCancel when the tools/call
	// returned; the detached registration must survive it.
	sess.UnregisterCancel(meta.RequestID)
	if !sess.Cancel(meta.RequestID) {
		t.Fatalf("detached cancel registration did not survive UnregisterCancel")
	}

	job := waitForTerminal(t, store, ack.JobID, 2*time.Second)
	if job.Status != jobs.JobStatusCancelled {
		t.Fatalf("job status = %s, want cancelled", job.Status)
	}
	var outcome types.ScreenshotWatchOutcome
	_ = json.Unmarshal(job.Result, &outcome)
	if outcome.StoppedReason != "cancelled" || outcome.FramesCaptured != 1 {
		t.Fatalf("outcome = %+v", outcome)
	}

	// The frame delivered before cancellation is still on the event stream.
	events := drainEvents(t, sess, 1, time.Second)
	if events[0]["frame"] == nil {
		t.Fatalf("partial frame event missing: %+v", events[0])
	}
}

func TestScreenshotWatch_AgentDisconnect_MarksFailed(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrConnectionClosed
	}}
	store := jobs.NewMemoryStore(0, nil)
	deps := newScreenshotDeps(dispatch, store)
	sess := session.New(context.Background(), "sess-1", 10)

	result, _ := callWatch(deps, sess, transport.ToolCallMeta{RequestID: "req-1"}, map[string]any{"clientId": "dev-1"})
	var ack types.ScreenshotWatchAck
	_ = json.Unmarshal(result.StructuredContent, &ack)

	job := waitForTerminal(t, store, ack.JobID, 2*time.Second)
	if job.Status != jobs.JobStatusFailed {
		t.Fatalf("job status = %s, want failed", job.Status)
	}
	var outcome types.ScreenshotWatchOutcome
	_ = json.Unmarshal(job.Result, &outcome)
	if outcome.StoppedReason != "agent_disconnect" {
		t.Fatalf("stoppedReason = %q, want agent_disconnect", outcome.StoppedReason)
	}
}

func TestScreenshotWatch_JobTimeout_CancelsDispatch(t *testing.T) {
	cancels := NewWatchCancels()
	store := jobs.NewMemoryStore(50*time.Millisecond, cancels.Cancel)
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		<-ctx.Done()
		return protocol.ResultPayload{}, ctx.Err()
	}}
	deps := ScreenshotDeps{
		Bridge: dispatch, Jobs: store, Cancels: cancels,
		Online: func(string) bool { return true },
		// Negative slack pushes the job's TimeoutSecs to 0 so the store's
		// 50ms default timeout governs.
		TimeoutSlack: -time.Hour,
	}
	sess := session.New(context.Background(), "sess-1", 10)

	result, _ := callWatch(deps, sess, transport.ToolCallMeta{RequestID: "req-1"}, map[string]any{"clientId": "dev-1"})
	var ack types.ScreenshotWatchAck
	_ = json.Unmarshal(result.StructuredContent, &ack)

	// The store times out the job (failed/"timeout") and invokes
	// cancels.Cancel, which cancels the dispatch context and unblocks the
	// watch goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job, err := store.Get(ack.JobID); err == nil && job.Status.IsTerminal() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job never reached a terminal state after store timeout")
}

func TestWatchCancels_CancelUnknownIsNoop(t *testing.T) {
	c := NewWatchCancels()
	c.Cancel("dev-1", "nope", "timeout") // must not panic
	var cancelled bool
	c.add("id-1", func() { cancelled = true })
	c.Cancel("dev-1", "id-1", "timeout")
	if !cancelled {
		t.Fatalf("registered cancel was not invoked")
	}
	c.remove("id-1")
}

func TestScreenshotWatch_DispatchGenericError_UsesDispatchMessage(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, errors.New("boom")
	}}
	store := jobs.NewMemoryStore(0, nil)
	deps := newScreenshotDeps(dispatch, store)
	sess := session.New(context.Background(), "sess-1", 10)

	result, _ := callWatch(deps, sess, transport.ToolCallMeta{RequestID: "req-1"}, map[string]any{"clientId": "dev-1"})
	var ack types.ScreenshotWatchAck
	_ = json.Unmarshal(result.StructuredContent, &ack)

	job := waitForTerminal(t, store, ack.JobID, 2*time.Second)
	if job.Status != jobs.JobStatusFailed || job.Error != "boom" {
		t.Fatalf("job = %+v, want failed with error 'boom'", job)
	}
}
