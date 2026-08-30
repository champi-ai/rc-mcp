package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
)

// DefaultEmitBackpressure is how long Bridge.Dispatch waits for a full
// Session.EventCh before dropping an event (Section 8: "blocks for up to
// 5 seconds, then drops the event and logs a warning").
const DefaultEmitBackpressure = 5 * time.Second

// ErrDeviceOffline is returned by Bridge.Dispatch when the target device
// has no active connection.
var ErrDeviceOffline = errors.New("agent: device offline")

// ErrConnectionClosed is returned by Bridge.Dispatch when the agent's
// WebSocket connection closes while a dispatch is still in flight
// (Section 13: "Agent disconnects mid-execution").
var ErrConnectionClosed = errors.New("agent: connection closed during dispatch")

// Bridge forwards a single tools/call dispatch to its target agent over
// WebSocket and correlates the agent's progress/result messages back to
// the originating MCP session's EventCh. One Bridge.Dispatch call is
// spawned per in-flight tools/call that needs an agent (Section 8,
// "Dispatch bridge").
type Bridge struct {
	Hub *Hub

	// EmitBackpressure overrides DefaultEmitBackpressure; zero uses the
	// default. Exposed for tests.
	EmitBackpressure time.Duration
}

// NewBridge constructs a Bridge over hub.
func NewBridge(hub *Hub) *Bridge {
	return &Bridge{Hub: hub}
}

// ProgressFunc is called for each progress message the agent sends while a
// dispatch is in flight (both JSON progress payloads and binary frames,
// e.g. shell stdout chunks). event/data are already shaped as an
// session.SSEEvent ready to hand to Session.Emit; tool code (e.g.
// shell_exec, issue #18) supplies the mapping from ProgressPayload/binary
// frame to that shape.
type ProgressFunc func(payload *protocol.ProgressPayload, binary *BinaryFrame)

// BinaryFrame is a demuxed binary WS frame delivered to a ProgressFunc.
type BinaryFrame struct {
	Header protocol.BinaryHeader
	Data   []byte
}

// Dispatch sends a "dispatch" envelope for tool to the agent identified by
// deviceID, correlated by correlationID (must be a UUIDv4 string; the
// caller mints it, typically from the MCP tools/call request). It blocks
// until a terminal "result" arrives, ctx is cancelled, or the connection is
// lost, invoking onProgress for every intermediate progress/binary frame.
//
// On ctx cancellation, Dispatch sends a "cancel" message to the agent and
// waits briefly for a final result before returning ctx.Err().
func (b *Bridge) Dispatch(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress ProgressFunc) (protocol.ResultPayload, error) {
	conn, ok := b.Hub.Connection(deviceID)
	if !ok {
		return protocol.ResultPayload{}, ErrDeviceOffline
	}

	ch, err := conn.registerPending(correlationID)
	if err != nil {
		return protocol.ResultPayload{}, err
	}
	defer conn.unregisterPending(correlationID)

	conn.trySend(protocol.Envelope{
		Type: protocol.MsgDispatch,
		ID:   correlationID,
		Ts:   time.Now().UTC(),
		Payload: protocol.DispatchPayload{
			Tool:      tool,
			RequestID: correlationID,
			SessionID: sessionID,
			Input:     input,
		},
	})

	return b.waitForResult(ctx, deviceID, correlationID, ch, onProgress)
}

func (b *Bridge) waitForResult(ctx context.Context, deviceID, correlationID string, ch chan bridgeMessage, onProgress ProgressFunc) (protocol.ResultPayload, error) {
	cancelled := false
	for {
		select {
		case <-ctx.Done():
			if cancelled {
				// Already sent cancel and given the agent its grace
				// period; give up.
				return protocol.ResultPayload{}, ctx.Err()
			}
			cancelled = true
			// Send cancel via the device's *current* connection -- it may
			// have reconnected (with a new Connection) since dispatch.
			if cur, ok := b.Hub.Connection(deviceID); ok {
				cur.trySend(protocol.Envelope{
					Type: protocol.MsgCancel,
					ID:   correlationID,
					Ts:   time.Now().UTC(),
					Payload: protocol.CancelPayload{
						Reason: "client_cancelled",
					},
				})
			}
			// Give the agent a short grace period to send a final result
			// reflecting the cancellation before giving up entirely.
			select {
			case msg, chOpen := <-ch:
				if r, done, ok := b.handleMessage(msg, chOpen, onProgress); done {
					return r, ok
				}
				return protocol.ResultPayload{}, ctx.Err()
			case <-time.After(2 * time.Second):
				return protocol.ResultPayload{}, ctx.Err()
			}
		case msg, chOpen := <-ch:
			if r, done, resErr := b.handleMessage(msg, chOpen, onProgress); done {
				return r, resErr
			}
		}
	}
}

// handleMessage processes one bridgeMessage. done is true if this message
// was terminal (a result, an error, or the channel closing) and the caller
// should return (r, err) as the Dispatch outcome.
func (b *Bridge) handleMessage(msg bridgeMessage, chOpen bool, onProgress ProgressFunc) (protocol.ResultPayload, bool, error) {
	if !chOpen {
		return protocol.ResultPayload{}, true, ErrConnectionClosed
	}

	if msg.Envelope != nil {
		switch msg.Envelope.Type {
		case protocol.MsgResult:
			result, err := decodePayload[protocol.ResultPayload](msg.Envelope.Payload)
			if err != nil {
				return protocol.ResultPayload{}, true, fmt.Errorf("agent: decode result: %w", err)
			}
			return result, true, nil
		case protocol.MsgError:
			errp, _ := decodePayload[protocol.ErrorPayload](msg.Envelope.Payload)
			return protocol.ResultPayload{}, true, fmt.Errorf("agent error %s: %s", errp.Code, errp.Message)
		case protocol.MsgProgress:
			if onProgress != nil {
				p, err := decodePayload[protocol.ProgressPayload](msg.Envelope.Payload)
				if err == nil {
					onProgress(&p, nil)
				}
			}
		}
		return protocol.ResultPayload{}, false, nil
	}

	// Binary frame.
	if onProgress != nil {
		onProgress(nil, &BinaryFrame{Header: msg.Header, Data: msg.Data})
	}
	return protocol.ResultPayload{}, false, nil
}

// EmitProgress is a convenience ProgressFunc builder for tool handlers: it
// wraps a JSON progress payload as an SSE notifications/progress event and
// forwards it to sess with the given progressToken, honoring the
// backpressure policy (Section 8).
func EmitProgress(sess *session.Session, progressToken string, backpressure time.Duration) ProgressFunc {
	if backpressure <= 0 {
		backpressure = DefaultEmitBackpressure
	}
	return func(payload *protocol.ProgressPayload, _ *BinaryFrame) {
		if payload == nil {
			return
		}
		data, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"method":  "notifications/progress",
			"params": map[string]any{
				"progressToken": progressToken,
				"message":       payload.Message,
				"percent":       payload.Percent,
			},
		})
		if err != nil {
			return
		}
		if !sess.Emit(session.SSEEvent{Data: string(data)}, backpressure) {
			log.Printf("session %s: dropped progress event for token %s (EventCh full)", sess.ID, progressToken)
		}
	}
}

// EmitProgressAndBinary is like EmitProgress but also bridges binary frames
// (PTY/shell stdout, screenshot PNG frames, file content chunks) to
// notifications/progress: the frame's raw bytes are base64-encoded into a
// "data" field alongside "frameType" so the client can tell payload kinds
// apart, per Section 9's two-hop streaming model.
func EmitProgressAndBinary(sess *session.Session, progressToken string, backpressure time.Duration) ProgressFunc {
	if backpressure <= 0 {
		backpressure = DefaultEmitBackpressure
	}
	return func(payload *protocol.ProgressPayload, binary *BinaryFrame) {
		var params map[string]any
		switch {
		case payload != nil:
			params = map[string]any{
				"progressToken": progressToken,
				"message":       payload.Message,
				"percent":       payload.Percent,
			}
		case binary != nil:
			params = map[string]any{
				"progressToken": progressToken,
				"data":          base64.StdEncoding.EncodeToString(binary.Data),
				"frameType":     int(binary.Header.FrameType),
			}
		default:
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
		if !sess.Emit(session.SSEEvent{Data: string(data)}, backpressure) {
			log.Printf("session %s: dropped progress event for token %s (EventCh full)", sess.ID, progressToken)
		}
	}
}
