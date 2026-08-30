// This file implements the server side of screenshot_capture (synchronous
// dispatch) and screenshot_watch (long-running dispatch pattern (a) with a
// job store record and pushed PNG frames). See docs/specs/backend.md
// Section 3.2 and Section 9.
package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/agent"
	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/jobs"
	"github.com/CloudKeter/rc-mcp/internal/mcp/types"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/session"
	"github.com/CloudKeter/rc-mcp/internal/transport"
)

const screenshotCaptureInputSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string", "description": "Target device ID" },
    "display":  { "type": "string", "description": "X11 display (default: :0)" },
    "monitor":  { "type": "integer", "description": "Monitor index (default: -1 = all monitors stitched)", "minimum": -1 },
    "quality":  { "type": "integer", "description": "PNG compression level 0-9 (default: 6)", "minimum": 0, "maximum": 9 },
    "maxWidth": { "type": "integer", "description": "Max width in px; image is downscaled preserving aspect ratio if exceeded" }
  },
  "required": ["clientId"]
}`

const screenshotWatchInputSchema = `{
  "type": "object",
  "properties": {
    "clientId":     { "type": "string", "description": "Target device ID" },
    "display":      { "type": "string", "description": "X11 display (default: :0)" },
    "monitor":      { "type": "integer", "description": "Monitor index (default: -1 = all)", "minimum": -1 },
    "intervalMs":   { "type": "integer", "description": "Capture interval in ms (default: 2000, min: 500)", "minimum": 500 },
    "maxFrames":    { "type": "integer", "description": "Max frames to capture (default: 30, max: 120)", "minimum": 1, "maximum": 120 },
    "durationSecs": { "type": "integer", "description": "Max duration in seconds (default: 60, max: 300)", "minimum": 1, "maximum": 300 },
    "maxWidth":     { "type": "integer", "description": "Max width in px for downscaling" },
    "quality":      { "type": "integer", "minimum": 0, "maximum": 9, "description": "PNG compression (default: 6)" }
  },
  "required": ["clientId"]
}`

const screenshotWatchOutputSchema = `{
  "type": "object",
  "properties": {
    "jobId":    { "type": "string" },
    "clientId": { "type": "string" }
  },
  "required": ["jobId", "clientId"]
}`

const screenshotAnnotations = `{
  "readOnlyHint": true,
  "destructiveHint": false,
  "idempotentHint": true,
  "openWorldHint": false
}`

// WatchCancels tracks the context CancelFuncs of in-flight detached watch
// jobs by correlation ID, so the job store's timeout policy (Section 9,
// JOB_TIMEOUT) can stop the dispatch -- cancelling the watch context makes
// the bridge send a "cancel" to the agent.
type WatchCancels struct {
	mu sync.Mutex
	m  map[string]context.CancelFunc
}

// NewWatchCancels constructs an empty WatchCancels registry.
func NewWatchCancels() *WatchCancels {
	return &WatchCancels{m: map[string]context.CancelFunc{}}
}

func (w *WatchCancels) add(correlationID string, cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.m[correlationID] = cancel
}

func (w *WatchCancels) remove(correlationID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.m, correlationID)
}

// Cancel satisfies jobs.CancelFunc's shape (wrapped in a closure in
// cmd/server): it cancels the watch context for correlationID, if one is
// still in flight.
func (w *WatchCancels) Cancel(clientID, correlationID, reason string) {
	w.mu.Lock()
	cancel, ok := w.m[correlationID]
	w.mu.Unlock()
	if ok {
		cancel()
	}
}

// ScreenshotDeps are the dependencies the screenshot tools need: the
// dispatch bridge, the job store (screenshot_watch only), the watch-cancel
// registry shared with the job store's timeout callback, and the audit
// logger.
type ScreenshotDeps struct {
	Bridge  ShellDispatcher
	Jobs    jobs.JobStore
	Cancels *WatchCancels
	Audit   *audit.Logger
	// Online reports whether the device currently has a live agent
	// connection; screenshot_watch uses it to fail fast (and not create a
	// job record) for offline devices. Nil skips the pre-flight check.
	Online func(clientID string) bool
	// TimeoutSlack is added to a watch's durationSecs to form the job's
	// TimeoutSecs, so the agent's terminal result normally wins the race
	// against JOB_TIMEOUT. Zero uses DefaultWatchTimeoutSlack; a value
	// that makes the total non-positive falls back to the job store's
	// default timeout.
	TimeoutSlack time.Duration
}

// DefaultWatchTimeoutSlack is the default extra headroom added to a
// screenshot_watch job's timeout beyond its own duration bound.
const DefaultWatchTimeoutSlack = 30 * time.Second

// NewScreenshotCaptureDefinition builds the screenshot_capture tool
// definition per docs/specs/backend.md Section 3.2.1.
func NewScreenshotCaptureDefinition(deps ScreenshotDeps) Definition {
	return Definition{
		Name:               "screenshot_capture",
		Title:              "Capture Screenshot",
		Description:        "Capture the current display on the target device and return it as a PNG image.",
		InputSchema:        json.RawMessage(screenshotCaptureInputSchema),
		Annotations:        json.RawMessage(screenshotAnnotations),
		RequiredCapability: "screenshot",
		Handler:            deps.handleCapture,
	}
}

// NewScreenshotWatchDefinition builds the screenshot_watch tool definition
// per docs/specs/backend.md Section 3.2.2 (dispatch pattern (a)).
func NewScreenshotWatchDefinition(deps ScreenshotDeps) Definition {
	return Definition{
		Name:               "screenshot_watch",
		Title:              "Watch Screen (Periodic Screenshots)",
		Description:        "Stream periodic screenshots from the target device as push notifications for a bounded duration.",
		InputSchema:        json.RawMessage(screenshotWatchInputSchema),
		OutputSchema:       json.RawMessage(screenshotWatchOutputSchema),
		Annotations:        json.RawMessage(screenshotAnnotations),
		RequiredCapability: "screenshot",
		Handler:            deps.handleWatch,
	}
}

func (d ScreenshotDeps) handleCapture(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.ScreenshotCaptureInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}
	if input.Quality != nil && (*input.Quality < 0 || *input.Quality > 9) {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"quality\" must be between 0 and 9"}
	}

	start := time.Now()
	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}

	// The agent sends the PNG bytes as a binary FrameScreenshotPNG frame;
	// the JSON result only carries metadata (types.ScreenshotCaptureOutput).
	var pngBytes []byte
	onProgress := func(payload *protocol.ProgressPayload, binary *agent.BinaryFrame) {
		if binary != nil && binary.Header.FrameType == protocol.FrameScreenshotPNG {
			pngBytes = append(pngBytes, binary.Data...)
		}
	}

	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, input.ClientID, correlationID, "screenshot_capture", sess.ID, input, onProgress)
	duration := time.Since(start)

	if dispatchErr != nil {
		d.logAudit(sess.ID, input.ClientID, "screenshot_capture", input, audit.StatusError, duration, dispatchErr.Error())
		return toolError(dispatchErrorMessage(dispatchErr, input.ClientID)), nil
	}
	if resultPayload.IsError {
		d.logAudit(sess.ID, input.ClientID, "screenshot_capture", input, audit.StatusError, duration, resultPayload.Error)
		return toolError(resultPayload.Error), nil
	}
	if len(pngBytes) == 0 {
		d.logAudit(sess.ID, input.ClientID, "screenshot_capture", input, audit.StatusError, duration, "no image frame received")
		return toolError("agent returned no image data"), nil
	}

	output, err := decodePayload[types.ScreenshotCaptureOutput](resultPayload.Output)
	if err != nil {
		d.logAudit(sess.ID, input.ClientID, "screenshot_capture", input, audit.StatusError, duration, err.Error())
		return toolError("failed to decode agent result"), nil
	}
	output.ClientID = input.ClientID
	if output.MimeType == "" {
		output.MimeType = "image/png"
	}

	d.logAudit(sess.ID, input.ClientID, "screenshot_capture", input, audit.StatusOK, duration, "")

	metaJSON, err := json.Marshal(output)
	if err != nil {
		return toolError("failed to encode result"), nil
	}
	return &transport.ToolCallResult{
		Content: []transport.ToolContent{
			{Type: "image", Data: base64.StdEncoding.EncodeToString(pngBytes), MimeType: output.MimeType},
			{Type: "text", Text: string(metaJSON)},
		},
		StructuredContent: metaJSON,
	}, nil
}

func (d ScreenshotDeps) handleWatch(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	var input types.ScreenshotWatchInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}
	if input.IntervalMs != nil && *input.IntervalMs < 500 {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"intervalMs\" must be >= 500"}
	}
	if input.MaxFrames != nil && (*input.MaxFrames < 1 || *input.MaxFrames > 120) {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"maxFrames\" must be between 1 and 120"}
	}
	if input.DurationSecs != nil && (*input.DurationSecs < 1 || *input.DurationSecs > 300) {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"durationSecs\" must be between 1 and 300"}
	}
	if input.Quality != nil && (*input.Quality < 0 || *input.Quality > 9) {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"quality\" must be between 0 and 9"}
	}

	// Idempotency (Section 9): a retry of the same tools/call returns the
	// existing job rather than starting a second watch.
	idemKey := jobs.IdempotencyKey(sess.ID, "screenshot_watch", meta.RequestID)
	if existing, ok := d.Jobs.FindByIdempotencyKey(idemKey); ok && existing.Status != jobs.JobStatusFailed {
		return marshalToolResult(types.ScreenshotWatchAck{JobID: existing.ID, ClientID: existing.ClientID})
	}

	if d.Online != nil && !d.Online(input.ClientID) {
		return toolError(fmt.Sprintf("Device %s is offline", input.ClientID)), nil
	}

	jobID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}

	durationSecs := 60
	if input.DurationSecs != nil {
		durationSecs = *input.DurationSecs
	}
	maxFrames := 30
	if input.MaxFrames != nil {
		maxFrames = *input.MaxFrames
	}

	payloadJSON, err := json.Marshal(input)
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}
	slack := d.TimeoutSlack
	if slack == 0 {
		slack = DefaultWatchTimeoutSlack
	}
	timeoutSecs := durationSecs + int(slack.Seconds())
	if timeoutSecs < 0 {
		timeoutSecs = 0 // job store default applies
	}
	job := &jobs.Job{
		ID:             jobID,
		SessionID:      sess.ID,
		ClientID:       input.ClientID,
		Tool:           "screenshot_watch",
		Status:         jobs.JobStatusPending,
		Payload:        payloadJSON,
		IdempotencyKey: idemKey,
		ProgressToken:  meta.ProgressToken,
		CorrelationID:  jobID,
		TimeoutSecs:    timeoutSecs,
	}
	if err := d.Jobs.Create(job); err != nil {
		if errors.Is(err, jobs.ErrDuplicateIdempotencyKey) {
			if existing, ok := d.Jobs.FindByIdempotencyKey(idemKey); ok {
				return marshalToolResult(types.ScreenshotWatchAck{JobID: existing.ID, ClientID: existing.ClientID})
			}
		}
		return nil, &transport.RPCError{Code: -32603, Message: "failed to create job"}
	}

	d.logAudit(sess.ID, input.ClientID, "screenshot_watch", input, audit.StatusOK, 0, "started")

	// Detached watch goroutine (pattern (a)): scoped to the session, not
	// the originating HTTP request, so it outlives this tools/call.
	watchCtx, cancelWatch := context.WithCancel(sess.Ctx)
	sess.DetachCancel(meta.RequestID, cancelWatch)
	if d.Cancels != nil {
		d.Cancels.add(jobID, cancelWatch)
	}
	go d.runWatch(watchCtx, cancelWatch, sess, meta, input, jobID, maxFrames)

	return marshalToolResult(types.ScreenshotWatchAck{JobID: jobID, ClientID: input.ClientID})
}

// runWatch drives one detached screenshot_watch dispatch to completion:
// it bridges each PNG frame to a notifications/progress event, then
// persists the terminal outcome in the job store and emits the terminal
// progress event (Section 9, dispatch pattern (a)).
func (d ScreenshotDeps) runWatch(ctx context.Context, cancel context.CancelFunc, sess *session.Session, meta transport.ToolCallMeta, input types.ScreenshotWatchInput, jobID string, maxFrames int) {
	start := time.Now()
	defer func() {
		cancel()
		sess.RemoveDetachedCancel(meta.RequestID)
		if d.Cancels != nil {
			d.Cancels.remove(jobID)
		}
	}()

	_ = d.Jobs.UpdateStatus(jobID, jobs.JobStatusRunning, nil, "")

	framesSent := 0
	onProgress := func(payload *protocol.ProgressPayload, binary *agent.BinaryFrame) {
		if binary == nil || binary.Header.FrameType != protocol.FrameScreenshotPNG {
			return
		}
		framesSent++
		percent := float64(framesSent) / float64(maxFrames) * 100
		d.emitProgress(sess, meta.ProgressToken, map[string]any{
			"progressToken": meta.ProgressToken,
			"message":       fmt.Sprintf("frame %d", framesSent),
			"percent":       percent,
			"frame": map[string]any{
				"type":     "image",
				"data":     base64.StdEncoding.EncodeToString(binary.Data),
				"mimeType": "image/png",
			},
		})
	}

	resultPayload, dispatchErr := d.Bridge.Dispatch(ctx, input.ClientID, jobID, "screenshot_watch", sess.ID, input, onProgress)
	duration := time.Since(start)

	outcome := types.ScreenshotWatchOutcome{
		JobID:          jobID,
		FramesCaptured: framesSent,
		DurationMs:     duration.Milliseconds(),
		ClientID:       input.ClientID,
	}
	status := jobs.JobStatusSucceeded
	errMsg := ""

	switch {
	case dispatchErr == nil && !resultPayload.IsError:
		if agentOutcome, err := decodePayload[types.ScreenshotWatchOutcome](resultPayload.Output); err == nil {
			outcome.FramesCaptured = agentOutcome.FramesCaptured
			outcome.StoppedReason = agentOutcome.StoppedReason
		}
		if outcome.StoppedReason == "cancelled" {
			status = jobs.JobStatusCancelled
		}
	case errors.Is(dispatchErr, context.Canceled):
		status = jobs.JobStatusCancelled
		outcome.StoppedReason = "cancelled"
	case errors.Is(dispatchErr, agent.ErrConnectionClosed):
		status = jobs.JobStatusFailed
		outcome.StoppedReason = "agent_disconnect"
		errMsg = dispatchErr.Error()
	default:
		status = jobs.JobStatusFailed
		outcome.StoppedReason = "error"
		if dispatchErr != nil {
			errMsg = dispatchErrorMessage(dispatchErr, input.ClientID)
		} else {
			errMsg = resultPayload.Error
		}
	}

	outcomeJSON, err := json.Marshal(outcome)
	if err != nil {
		outcomeJSON = nil
	}
	if err := d.Jobs.UpdateStatus(jobID, status, outcomeJSON, errMsg); err != nil {
		log.Printf("screenshot_watch %s: failed to persist terminal status: %v", jobID, err)
	}

	auditStatus := audit.StatusOK
	if status == jobs.JobStatusFailed {
		auditStatus = audit.StatusError
	} else if status == jobs.JobStatusCancelled {
		auditStatus = audit.StatusCancelled
	}
	d.logAudit(sess.ID, input.ClientID, "screenshot_watch", input, auditStatus, duration, errMsg)

	// Terminal progress event: carries the job outcome so a connected
	// client learns the summary without a resources/read.
	d.emitProgress(sess, meta.ProgressToken, map[string]any{
		"progressToken": meta.ProgressToken,
		"message":       "completed",
		"percent":       100.0,
		"result":        outcome,
	})
}

// emitProgress wraps params as a notifications/progress JSON-RPC message
// and emits it on the session's SSE stream with the standard backpressure
// policy (Section 8).
func (d ScreenshotDeps) emitProgress(sess *session.Session, progressToken string, params map[string]any) {
	if progressToken == "" {
		return
	}
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params":  params,
	})
	if err != nil {
		return
	}
	if !sess.Emit(session.SSEEvent{Data: string(data)}, agent.DefaultEmitBackpressure) {
		log.Printf("session %s: dropped screenshot progress event for token %s (EventCh full)", sess.ID, progressToken)
	}
}

func (d ScreenshotDeps) logAudit(sessionID, clientID, tool string, input any, status string, duration time.Duration, errMsg string) {
	if d.Audit == nil {
		return
	}
	_ = d.Audit.LogCall(sessionID, clientID, tool, input, status, duration, errMsg)
}
