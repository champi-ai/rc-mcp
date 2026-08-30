package client

import (
	"context"
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/champi-ai/rc-mcp/agent/executor"
	"github.com/champi-ai/rc-mcp/internal/mcp/types"
	"github.com/champi-ai/rc-mcp/internal/protocol"
)

// Dispatcher routes incoming "dispatch" messages to the correct tool
// executor by tool name, tracks in-flight dispatches so a "cancel" message
// can stop them, and re-validates every dispatch payload before executing
// it (defense-in-depth: the server already validated, but the agent never
// trusts the wire). See docs/specs/backend.md Section 2.2 and Section 12.6.
type Dispatcher struct {
	mu       sync.Mutex
	inFlight map[string]context.CancelFunc // correlation ID -> cancel

	shellSessions *executor.ShellSessionManager
}

// NewDispatcher constructs an empty Dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		inFlight:      map[string]context.CancelFunc{},
		shellSessions: executor.NewShellSessionManager(),
	}
}

// HandleDispatch processes one "dispatch" envelope and returns the
// terminal "result" envelope to send back. sendBinary, if non-nil, is
// called with each raw binary WS frame (BinaryHeader + payload) as
// streaming output becomes available (e.g. shell stdout chunks); its
// errors are not fatal to the dispatch (a dropped progress frame doesn't
// abort a running command).
func (d *Dispatcher) HandleDispatch(ctx context.Context, env protocol.Envelope, sendBinary func([]byte) error) protocol.Envelope {
	dispatch, err := decodePayload[protocol.DispatchPayload](env.Payload)
	if err != nil {
		return errorResult(env.ID, "", "invalid_payload")
	}

	switch dispatch.Tool {
	case "shell_exec":
		return d.handleShellExec(ctx, env.ID, dispatch, sendBinary)
	case "shell_session_start":
		return d.handleShellSessionStart(env.ID, dispatch)
	case "shell_session_write":
		return d.handleShellSessionWrite(ctx, env.ID, dispatch, sendBinary)
	case "shell_session_close":
		return d.handleShellSessionClose(env.ID, dispatch)
	case "screenshot_capture":
		return d.handleScreenshotCapture(env.ID, dispatch, sendBinary)
	case "screenshot_watch":
		return d.handleScreenshotWatch(ctx, env.ID, dispatch, sendBinary)
	case "fs_read":
		return d.handleFSRead(env.ID, dispatch, sendBinary)
	case "fs_write":
		return d.handleFSWrite(env.ID, dispatch)
	case "fs_list":
		return d.handleFSList(env.ID, dispatch)
	case "fs_delete":
		return d.handleFSDelete(env.ID, dispatch)
	case "fs_stat":
		return d.handleFSStat(env.ID, dispatch)
	case "process_list":
		return d.handleProcessList(env.ID, dispatch)
	case "process_info":
		return d.handleProcessInfo(env.ID, dispatch)
	case "process_signal":
		return d.handleProcessSignal(env.ID, dispatch)
	case "sysinfo_get":
		return d.handleSysinfoGet(env.ID, dispatch)
	case "input_key":
		return d.handleInputKey(env.ID, dispatch, sendBinary)
	case "input_mouse_click":
		return d.handleInputMouseClick(env.ID, dispatch, sendBinary)
	case "input_mouse_move":
		return d.handleInputMouseMove(env.ID, dispatch, sendBinary)
	case "input_type":
		return d.handleInputType(env.ID, dispatch, sendBinary)
	default:
		return errorResult(env.ID, dispatch.Tool, "unknown_tool")
	}
}

// HandleCancel processes a "cancel" envelope by cancelling the context of
// the in-flight dispatch it correlates to, if any. It is a no-op if no
// dispatch with that correlation ID is currently running.
func (d *Dispatcher) HandleCancel(env protocol.Envelope) {
	d.mu.Lock()
	cancel, ok := d.inFlight[env.ID]
	d.mu.Unlock()
	if ok {
		cancel()
	}
}

func (d *Dispatcher) register(correlationID string, cancel context.CancelFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inFlight[correlationID] = cancel
}

// CancelAll cancels every in-flight dispatch (reconnect grace expired, or
// the server said resume:false -- its side of those dispatches is gone).
func (d *Dispatcher) CancelAll() {
	d.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(d.inFlight))
	for _, cancel := range d.inFlight {
		cancels = append(cancels, cancel)
	}
	d.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// CloseAllShellSessions kills every local shell session and releases its
// PTY (Section 2.1: beyond the grace period, orphaned sessions are
// cleaned up).
func (d *Dispatcher) CloseAllShellSessions() {
	d.shellSessions.CloseAll()
}

func (d *Dispatcher) unregister(correlationID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inFlight, correlationID)
}

func (d *Dispatcher) handleShellExec(ctx context.Context, correlationID string, dispatch protocol.DispatchPayload, sendBinary func([]byte) error) protocol.Envelope {
	input, err := decodePayload[types.ShellExecInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}

	// Defense-in-depth re-validation: never trust that the server's
	// validation was sufficient or that the wire wasn't tampered with.
	if strings.TrimSpace(input.Command) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: command")
	}
	if input.Timeout != nil && (*input.Timeout < 1 || *input.Timeout > 300) {
		return errorResult(correlationID, dispatch.Tool, "timeout out of range [1,300]")
	}

	execCtx, cancel := context.WithCancel(ctx)
	d.register(correlationID, cancel)
	defer func() {
		cancel()
		d.unregister(correlationID)
	}()

	prefix, prefixErr := protocol.CorrelationPrefix(correlationID)
	var seq uint32
	onChunk := func(chunk []byte) {
		if sendBinary == nil || prefixErr != nil {
			return
		}
		buf := make([]byte, protocol.BinaryHeaderSize+len(chunk))
		protocol.EncodeBinaryHeader(buf, protocol.BinaryHeader{
			CorrelationPrefix: prefix,
			StreamSeq:         seq,
			FrameType:         protocol.FrameShellStdout,
		})
		copy(buf[protocol.BinaryHeaderSize:], chunk)
		seq++
		_ = sendBinary(buf)
	}

	result, err := executor.Exec(execCtx, input, onChunk)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.ShellExecOutput{
				Stdout:     result.Stdout,
				Stderr:     result.Stderr,
				ExitCode:   result.ExitCode,
				Killed:     result.Killed,
				DurationMs: result.DurationMs,
			},
		},
	}
}

func (d *Dispatcher) handleShellSessionStart(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.ShellSessionStartInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}

	sess, err := d.shellSessions.Start(input.ClientID, input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.ShellSessionStartOutput{
				ShellSessionID: sess.ID,
				PID:            sess.PID,
				Shell:          sess.Shell,
				ClientID:       sess.ClientID,
			},
		},
	}
}

func (d *Dispatcher) handleShellSessionWrite(ctx context.Context, correlationID string, dispatch protocol.DispatchPayload, sendBinary func([]byte) error) protocol.Envelope {
	input, err := decodePayload[types.ShellSessionWriteInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ShellSessionID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: shellSessionId")
	}

	sess, err := d.shellSessions.Get(input.ShellSessionID)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "shell session not found or already closed")
	}

	bytesWritten, err := sess.Write(input.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	writeCtx, cancel := context.WithCancel(ctx)
	d.register(correlationID, cancel)
	defer func() {
		cancel()
		d.unregister(correlationID)
	}()

	prefix, prefixErr := protocol.CorrelationPrefix(correlationID)
	var seq uint32
	onChunk := func(chunk []byte) {
		if sendBinary == nil || prefixErr != nil {
			return
		}
		buf := make([]byte, protocol.BinaryHeaderSize+len(chunk))
		protocol.EncodeBinaryHeader(buf, protocol.BinaryHeader{
			CorrelationPrefix: prefix,
			StreamSeq:         seq,
			FrameType:         protocol.FrameShellStdout,
		})
		copy(buf[protocol.BinaryHeaderSize:], chunk)
		seq++
		_ = sendBinary(buf)
	}

	output, exited, exitCode := sess.StreamUntilIdleOrTimeout(writeCtx, onChunk, executor.DefaultShellSessionIdleTimeout, executor.DefaultShellSessionReadTimeout)

	out := types.ShellSessionWriteOutput{
		BytesWritten: bytesWritten,
		Output:       output,
		Exited:       exited,
	}
	if exited {
		ec := exitCode
		out.ExitCode = &ec
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool:   dispatch.Tool,
			Output: out,
		},
	}
}

func (d *Dispatcher) handleShellSessionClose(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.ShellSessionCloseInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ShellSessionID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: shellSessionId")
	}

	signal := "SIGTERM"
	if input.Signal != nil && *input.Signal != "" {
		signal = *input.Signal
	}

	exitCode, finalOutput, err := d.shellSessions.Close(input.ShellSessionID, signal, executor.DefaultShellSessionCloseGrace)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "shell session not found")
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.ShellSessionCloseOutput{
				ExitCode:    exitCode,
				FinalOutput: finalOutput,
			},
		},
	}
}

func (d *Dispatcher) handleScreenshotCapture(correlationID string, dispatch protocol.DispatchPayload, sendBinary func([]byte) error) protocol.Envelope {
	input, err := decodePayload[types.ScreenshotCaptureInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}

	display := ""
	if input.Display != nil {
		display = *input.Display
	}
	maxWidth := 0
	if input.MaxWidth != nil {
		maxWidth = *input.MaxWidth
	}
	quality := executor.DefaultScreenshotQuality
	if input.Quality != nil {
		quality = *input.Quality
	}

	pngBytes, width, height, err := executor.CaptureScreenshot(display, maxWidth, quality)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	sendPNGFrame(sendBinary, correlationID, 0, pngBytes)

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.ScreenshotCaptureOutput{
				Width:    width,
				Height:   height,
				MimeType: "image/png",
				ClientID: input.ClientID,
			},
		},
	}
}

func (d *Dispatcher) handleScreenshotWatch(ctx context.Context, correlationID string, dispatch protocol.DispatchPayload, sendBinary func([]byte) error) protocol.Envelope {
	input, err := decodePayload[types.ScreenshotWatchInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}

	watchCtx, cancel := context.WithCancel(ctx)
	d.register(correlationID, cancel)
	defer func() {
		cancel()
		d.unregister(correlationID)
	}()

	opts := executor.WatchOptions{}
	if input.Display != nil {
		opts.Display = *input.Display
	}
	if input.MaxWidth != nil {
		opts.MaxWidth = *input.MaxWidth
	}
	if input.Quality != nil {
		opts.Quality = *input.Quality
	}
	if input.IntervalMs != nil {
		opts.IntervalMs = *input.IntervalMs
	}
	if input.MaxFrames != nil {
		opts.MaxFrames = *input.MaxFrames
	}
	if input.DurationSecs != nil {
		opts.DurationSecs = *input.DurationSecs
	}

	result, err := executor.WatchScreenshots(watchCtx, opts, func(frame executor.WatchFrame) {
		sendPNGFrame(sendBinary, correlationID, uint32(frame.Index), frame.PNG)
	})
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.ScreenshotWatchOutcome{
				JobID:          correlationID,
				FramesCaptured: result.FramesCaptured,
				DurationMs:     result.DurationMs,
				StoppedReason:  result.StoppedReason,
				ClientID:       input.ClientID,
			},
		},
	}
}

func sendPNGFrame(sendBinary func([]byte) error, correlationID string, seq uint32, data []byte) {
	if sendBinary == nil {
		return
	}
	prefix, err := protocol.CorrelationPrefix(correlationID)
	if err != nil {
		return
	}
	buf := make([]byte, protocol.BinaryHeaderSize+len(data))
	protocol.EncodeBinaryHeader(buf, protocol.BinaryHeader{
		CorrelationPrefix: prefix,
		StreamSeq:         seq,
		FrameType:         protocol.FrameScreenshotPNG,
	})
	copy(buf[protocol.BinaryHeaderSize:], data)
	_ = sendBinary(buf)
}

func (d *Dispatcher) handleFSRead(correlationID string, dispatch protocol.DispatchPayload, sendBinary func([]byte) error) protocol.Envelope {
	input, err := decodePayload[types.FSReadInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}
	if strings.TrimSpace(input.Path) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: path")
	}

	var offset, limit int64
	if input.Offset != nil {
		offset = *input.Offset
	}
	if input.Limit != nil {
		limit = *input.Limit
	}
	encoding := ""
	if input.Encoding != nil {
		encoding = *input.Encoding
	}

	result, err := executor.FSRead(input.Path, offset, limit, encoding)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	out := types.FSReadOutput{
		Encoding:  result.Encoding,
		Size:      result.Size,
		Truncated: result.Truncated,
		ClientID:  input.ClientID,
	}

	if len(result.Content) >= executor.FSChunkThreshold {
		// Stream as binary FrameFileContent chunks; leave Content empty so
		// the server assembles it from the streamed frames instead
		// (Section 3.3.1).
		streamFileContent(sendBinary, correlationID, result.Content)
	} else if result.Encoding == "base64" {
		out.Content = base64.StdEncoding.EncodeToString(result.Content)
	} else {
		out.Content = string(result.Content)
	}

	return protocol.Envelope{
		Type:    protocol.MsgResult,
		ID:      correlationID,
		Ts:      time.Now().UTC(),
		Payload: protocol.ResultPayload{Tool: dispatch.Tool, Output: out},
	}
}

func streamFileContent(sendBinary func([]byte) error, correlationID string, data []byte) {
	if sendBinary == nil {
		return
	}
	prefix, err := protocol.CorrelationPrefix(correlationID)
	if err != nil {
		return
	}
	var seq uint32
	for off := 0; off < len(data); off += executor.FSStreamChunkSize {
		end := off + executor.FSStreamChunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]
		buf := make([]byte, protocol.BinaryHeaderSize+len(chunk))
		protocol.EncodeBinaryHeader(buf, protocol.BinaryHeader{
			CorrelationPrefix: prefix,
			StreamSeq:         seq,
			FrameType:         protocol.FrameFileContent,
		})
		copy(buf[protocol.BinaryHeaderSize:], chunk)
		seq++
		_ = sendBinary(buf)
	}
}

func (d *Dispatcher) handleFSWrite(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.FSWriteInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}
	if strings.TrimSpace(input.Path) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: path")
	}

	var content []byte
	if input.Encoding != nil && *input.Encoding == "base64" {
		content, err = base64.StdEncoding.DecodeString(input.Content)
		if err != nil {
			return errorResult(correlationID, dispatch.Tool, "invalid base64 content")
		}
	} else {
		content = []byte(input.Content)
	}

	mode := "overwrite"
	if input.Mode != nil && *input.Mode != "" {
		mode = *input.Mode
	}

	fileMode := os.FileMode(executor.DefaultFSFileMode)
	if input.FileMode != nil && *input.FileMode != "" {
		if parsed, err := strconv.ParseUint(*input.FileMode, 8, 32); err == nil {
			fileMode = os.FileMode(parsed)
		}
	}

	createDirs := true
	if input.CreateDirs != nil {
		createDirs = *input.CreateDirs
	}

	bytesWritten, absPath, err := executor.FSWrite(input.Path, content, mode, fileMode, createDirs)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.FSWriteOutput{
				BytesWritten: bytesWritten,
				Path:         absPath,
				ClientID:     input.ClientID,
			},
		},
	}
}

func (d *Dispatcher) handleFSList(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.FSListInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}
	if strings.TrimSpace(input.Path) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: path")
	}

	recursive := input.Recursive != nil && *input.Recursive
	maxDepth := 0
	if input.MaxDepth != nil {
		maxDepth = *input.MaxDepth
	}
	showHidden := input.ShowHidden != nil && *input.ShowHidden
	limit := 0
	if input.Limit != nil {
		limit = *input.Limit
	}

	result, err := executor.FSList(input.Path, recursive, maxDepth, showHidden, limit)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	entries := make([]types.FSEntry, 0, len(result.Entries))
	for _, e := range result.Entries {
		entries = append(entries, types.FSEntry{
			Name: e.Name, Path: e.Path, Type: e.Type, Size: e.Size, Mode: e.Mode, ModTime: e.ModTime,
		})
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.FSListOutput{
				Entries:    entries,
				Truncated:  result.Truncated,
				TotalCount: result.TotalCount,
				ClientID:   input.ClientID,
			},
		},
	}
}

func (d *Dispatcher) handleFSDelete(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.FSDeleteInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}
	if strings.TrimSpace(input.Path) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: path")
	}

	recursive := input.Recursive != nil && *input.Recursive

	result, err := executor.FSDelete(input.Path, recursive)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.FSDeleteOutput{
				Deleted:      true,
				Path:         input.Path,
				ItemsRemoved: result.ItemsRemoved,
				ClientID:     input.ClientID,
			},
		},
	}
}

func (d *Dispatcher) handleFSStat(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.FSStatInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}
	if strings.TrimSpace(input.Path) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: path")
	}

	followSymlinks := input.FollowSymlinks == nil || *input.FollowSymlinks

	result, err := executor.FSStat(input.Path, followSymlinks)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.FSStatOutput{
				Name:       result.Name,
				Path:       result.Path,
				Type:       result.Type,
				Size:       result.Size,
				Mode:       result.Mode,
				ModTime:    result.ModTime,
				Owner:      result.Owner,
				Group:      result.Group,
				LinkTarget: result.LinkTarget,
				ClientID:   input.ClientID,
			},
		},
	}
}

func (d *Dispatcher) handleProcessList(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.ProcessListInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}

	filter, userFilter, sortBy := "", "", ""
	if input.Filter != nil {
		filter = *input.Filter
	}
	if input.User != nil {
		userFilter = *input.User
	}
	if input.SortBy != nil {
		sortBy = *input.SortBy
	}
	limit := 0
	if input.Limit != nil {
		limit = *input.Limit
	}

	procs, total, err := executor.ListProcesses(filter, userFilter, sortBy, limit)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	summaries := make([]types.ProcessSummary, 0, len(procs))
	for _, p := range procs {
		summaries = append(summaries, types.ProcessSummary{
			PID: p.PID, PPID: p.PPID, Name: p.Name, Cmdline: p.Cmdline, User: p.User,
			CPUPct: p.CPUPct, MemPct: p.MemPct, MemRSSKB: p.MemRSSKB, State: p.State,
			StartTime: p.StartTime.Format(time.RFC3339),
		})
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.ProcessListOutput{
				Processes:  summaries,
				TotalCount: total,
				ClientID:   input.ClientID,
			},
		},
	}
}

func (d *Dispatcher) handleProcessInfo(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.ProcessInfoInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}

	info, err := executor.GetProcessInfo(input.PID)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.ProcessInfoOutput{
				PID: info.PID, PPID: info.PPID, Name: info.Name, Cmdline: info.Cmdline,
				Exe: info.Exe, Cwd: info.Cwd, User: info.User, State: info.State,
				Threads: info.Threads, CPUPct: info.CPUPct, MemPct: info.MemPct,
				MemRSSKB: info.MemRSSKB, MemVMSKB: info.MemVMSKB,
				StartTime: info.StartTime.Format(time.RFC3339),
				FDs:       info.FDs, Environ: info.Environ,
				ClientID: input.ClientID,
			},
		},
	}
}

func (d *Dispatcher) handleProcessSignal(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.ProcessSignalInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}

	signal := ""
	if input.Signal != nil {
		signal = *input.Signal
	}

	resolvedSignal, err := executor.SendProcessSignal(input.PID, signal)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}

	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool: dispatch.Tool,
			Output: types.ProcessSignalOutput{
				SignalSent: true,
				PID:        input.PID,
				Signal:     resolvedSignal,
				ClientID:   input.ClientID,
			},
		},
	}
}

func (d *Dispatcher) handleSysinfoGet(correlationID string, dispatch protocol.DispatchPayload) protocol.Envelope {
	input, err := decodePayload[types.SysinfoGetInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}

	sys := executor.GatherSysinfo(input.Sections)

	out := types.SysinfoGetOutput{Hostname: sys.Hostname, ClientID: input.ClientID}
	if sys.OS != nil {
		out.OS = &types.SysinfoOS{Name: sys.OS.Name, Version: sys.OS.Version, Kernel: sys.OS.Kernel, Arch: sys.OS.Arch}
	}
	if sys.Uptime != nil {
		out.Uptime = &types.SysinfoUptime{Seconds: sys.Uptime.Seconds, Human: sys.Uptime.Human}
	}
	if sys.CPU != nil {
		out.CPU = &types.SysinfoCPU{
			Model: sys.CPU.Model, Cores: sys.CPU.Cores, Threads: sys.CPU.Threads,
			UsagePct: sys.CPU.UsagePct, LoadAvg1: sys.CPU.LoadAvg1, LoadAvg5: sys.CPU.LoadAvg5, LoadAvg15: sys.CPU.LoadAvg15,
		}
	}
	if sys.Memory != nil {
		out.Memory = &types.SysinfoMemory{
			TotalKB: sys.Memory.TotalKB, UsedKB: sys.Memory.UsedKB, AvailableKB: sys.Memory.AvailableKB,
			UsagePct: sys.Memory.UsagePct, SwapTotalKB: sys.Memory.SwapTotalKB, SwapUsedKB: sys.Memory.SwapUsedKB,
		}
	}
	for _, dsk := range sys.Disk {
		out.Disk = append(out.Disk, types.SysinfoDisk{
			Mount: dsk.Mount, Device: dsk.Device, FSType: dsk.FSType,
			TotalKB: dsk.TotalKB, UsedKB: dsk.UsedKB, AvailableKB: dsk.AvailableKB, UsagePct: dsk.UsagePct,
		})
	}
	for _, n := range sys.Network {
		out.Network = append(out.Network, types.SysinfoNetworkIface{
			Name: n.Name, IPv4: n.IPv4, IPv6: n.IPv6, MAC: n.MAC, State: n.State,
		})
	}

	return protocol.Envelope{
		Type:    protocol.MsgResult,
		ID:      correlationID,
		Ts:      time.Now().UTC(),
		Payload: protocol.ResultPayload{Tool: dispatch.Tool, Output: out},
	}
}

// sendInputAck sends a single-byte FrameInputAck binary frame (1 =
// success, 0 = failure) as the low-level action acknowledgment for the
// input_* tools (Section 19, protocol version 2's FrameInputAck), ahead
// of the terminal JSON result.
func sendInputAck(sendBinary func([]byte) error, correlationID string, ok bool) {
	if sendBinary == nil {
		return
	}
	prefix, err := protocol.CorrelationPrefix(correlationID)
	if err != nil {
		return
	}
	var ack byte
	if ok {
		ack = 1
	}
	buf := make([]byte, protocol.BinaryHeaderSize+1)
	protocol.EncodeBinaryHeader(buf, protocol.BinaryHeader{
		CorrelationPrefix: prefix,
		StreamSeq:         0,
		FrameType:         protocol.FrameInputAck,
	})
	buf[protocol.BinaryHeaderSize] = ack
	_ = sendBinary(buf)
}

func (d *Dispatcher) handleInputKey(correlationID string, dispatch protocol.DispatchPayload, sendBinary func([]byte) error) protocol.Envelope {
	input, err := decodePayload[types.InputKeyInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}
	if strings.TrimSpace(input.Key) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: key")
	}

	err = executor.InputKey(input.Key)
	sendInputAck(sendBinary, correlationID, err == nil)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}
	return protocol.Envelope{
		Type: protocol.MsgResult, ID: correlationID, Ts: time.Now().UTC(),
		Payload: protocol.ResultPayload{Tool: dispatch.Tool, Output: types.InputKeyOutput{ClientID: input.ClientID}},
	}
}

func (d *Dispatcher) handleInputMouseClick(correlationID string, dispatch protocol.DispatchPayload, sendBinary func([]byte) error) protocol.Envelope {
	input, err := decodePayload[types.InputMouseClickInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}
	button := ""
	if input.Button != nil {
		button = *input.Button
	}

	err = executor.InputMouseClick(input.X, input.Y, button)
	sendInputAck(sendBinary, correlationID, err == nil)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}
	return protocol.Envelope{
		Type: protocol.MsgResult, ID: correlationID, Ts: time.Now().UTC(),
		Payload: protocol.ResultPayload{Tool: dispatch.Tool, Output: types.InputMouseClickOutput{ClientID: input.ClientID}},
	}
}

func (d *Dispatcher) handleInputMouseMove(correlationID string, dispatch protocol.DispatchPayload, sendBinary func([]byte) error) protocol.Envelope {
	input, err := decodePayload[types.InputMouseMoveInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}

	err = executor.InputMouseMove(input.X, input.Y)
	sendInputAck(sendBinary, correlationID, err == nil)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}
	return protocol.Envelope{
		Type: protocol.MsgResult, ID: correlationID, Ts: time.Now().UTC(),
		Payload: protocol.ResultPayload{Tool: dispatch.Tool, Output: types.InputMouseMoveOutput{ClientID: input.ClientID}},
	}
}

func (d *Dispatcher) handleInputType(correlationID string, dispatch protocol.DispatchPayload, sendBinary func([]byte) error) protocol.Envelope {
	input, err := decodePayload[types.InputTypeInput](dispatch.Input)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, "invalid_payload")
	}
	if strings.TrimSpace(input.ClientID) == "" {
		return errorResult(correlationID, dispatch.Tool, "missing required field: clientId")
	}

	err = executor.InputType(input.Text)
	sendInputAck(sendBinary, correlationID, err == nil)
	if err != nil {
		return errorResult(correlationID, dispatch.Tool, err.Error())
	}
	return protocol.Envelope{
		Type: protocol.MsgResult, ID: correlationID, Ts: time.Now().UTC(),
		Payload: protocol.ResultPayload{Tool: dispatch.Tool, Output: types.InputTypeOutput{ClientID: input.ClientID}},
	}
}

func errorResult(correlationID, tool, errMsg string) protocol.Envelope {
	return protocol.Envelope{
		Type: protocol.MsgResult,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.ResultPayload{
			Tool:    tool,
			IsError: true,
			Error:   errMsg,
		},
	}
}

// HandleDispatch is retained for compatibility with any external caller of
// the Phase 0 stub API: it delegates to a fresh Dispatcher with no
// streaming and no cancellation support. New code should construct a
// Dispatcher directly.
func HandleDispatch(env protocol.Envelope) protocol.Envelope {
	return NewDispatcher().HandleDispatch(context.Background(), env, nil)
}
