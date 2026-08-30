// This file implements cross-replica dispatch routing (docs/specs/
// backend.md Section 10, Section 19: "When a broker would be needed"):
// when an MCP session lives on one server replica but the agent it needs
// to reach is connected to a different replica, a ReplicaBridge routes the
// dispatch through Redis Pub/Sub to the replica that actually holds the
// agent's WebSocket connection, and routes progress/result messages back.
//
// This only activates in multi-replica deployments (MCP_SESSION_STORE=
// redis, cmd/server/main.go); a single-instance deployment never
// constructs a ReplicaBridge and pays none of this cost.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/redisclient"
)

// DefaultLocationTTL bounds how long a device-location record survives
// without a heartbeat refresh (locationHeartbeatLoop). Generous relative
// to the heartbeat interval so a single missed refresh under load doesn't
// make other replicas think the device went offline.
const DefaultLocationTTL = 45 * time.Second

const locationKeyPrefix = "rc-mcp:agent-location:"

func locationKey(deviceID string) string { return locationKeyPrefix + deviceID }

// LocationTracker records, in a shared KVStore, which replica currently
// holds each device's live WebSocket connection.
type LocationTracker struct {
	KV          redisclient.KVStore
	ReplicaID   string
	LocationTTL time.Duration
}

func (t *LocationTracker) ttl() time.Duration {
	if t.LocationTTL > 0 {
		return t.LocationTTL
	}
	return DefaultLocationTTL
}

// MarkOnline records that this replica now holds deviceID's connection.
func (t *LocationTracker) MarkOnline(ctx context.Context, deviceID string) error {
	return t.KV.Set(ctx, locationKey(deviceID), t.ReplicaID, t.ttl())
}

// MarkOffline clears the location record, but only if it still points at
// this replica -- a stale disconnect callback must never clobber a record
// a different (newer) connection to the same device already wrote.
func (t *LocationTracker) MarkOffline(ctx context.Context, deviceID string) {
	owner, err := t.KV.Get(ctx, locationKey(deviceID))
	if err != nil || owner != t.ReplicaID {
		return
	}
	_ = t.KV.Del(ctx, locationKey(deviceID))
}

// Owner returns the replica ID currently holding deviceID's connection, or
// ok=false if no replica does (or the record expired).
func (t *LocationTracker) Owner(ctx context.Context, deviceID string) (replicaID string, ok bool) {
	v, err := t.KV.Get(ctx, locationKey(deviceID))
	if err != nil {
		return "", false
	}
	return v, true
}

// Refresh extends the TTL of every currently-locally-connected device's
// location record. Intended to run on a ticker (see
// ReplicaBridge.RunLocationHeartbeat) so a long-lived connection's record
// doesn't expire out from under it between MarkOnline calls.
func (t *LocationTracker) Refresh(ctx context.Context, deviceIDs []string) {
	for _, id := range deviceIDs {
		_ = t.KV.Expire(ctx, locationKey(id), t.ttl())
	}
}

// requestChannel is where dispatch requests for deviceIDs owned by
// replicaID are published; replyChannel is where the owning replica
// publishes progress/binary/result messages back for one specific
// dispatch, identified by its correlation ID.
func requestChannel(replicaID string) string   { return "rc-mcp:relay:req:" + replicaID }
func replyChannel(correlationID string) string { return "rc-mcp:relay:reply:" + correlationID }

// relayRequest is published on a replica's request channel to ask it to
// perform a dispatch on this requester's behalf.
type relayRequest struct {
	DeviceID      string          `json:"deviceId"`
	CorrelationID string          `json:"correlationId"`
	Tool          string          `json:"tool"`
	SessionID     string          `json:"sessionId"`
	Input         json.RawMessage `json:"input"`
}

// relayCancel is published on a replica's request channel to cancel an
// in-flight relayed dispatch.
type relayCancel struct {
	Cancel        bool   `json:"cancel"`
	CorrelationID string `json:"correlationId"`
}

// relayMessage is published on a reply channel: exactly one of Progress,
// Binary, Result, or Err is set, mirroring bridgeMessage's shape for a
// local dispatch (Section 8) over the wire instead of a Go channel.
type relayMessage struct {
	Progress     *protocol.ProgressPayload `json:"progress,omitempty"`
	BinaryHeader *protocol.BinaryHeader    `json:"binaryHeader,omitempty"`
	BinaryData   []byte                    `json:"binaryData,omitempty"`
	Result       *protocol.ResultPayload   `json:"result,omitempty"`
	// Err, if non-empty, marks this as a terminal failure message
	// (mirrors Bridge's ErrConnectionClosed/dispatch-error path); Result
	// non-nil marks a terminal success. Neither set means this is an
	// intermediate progress/binary message.
	Err string `json:"err,omitempty"`
}

// ReplicaBridge wraps a local *Bridge with cross-replica routing: a
// dispatch to a device connected to this replica goes straight through
// Local (the existing zero-overhead in-process path); a dispatch to a
// device connected to a different replica is relayed via PubSub.
//
// ReplicaBridge implements the same Dispatch signature as *Bridge, so it
// is a drop-in replacement wherever a tool's Deps takes a dispatcher.
type ReplicaBridge struct {
	Local     *Bridge
	Hub       *Hub
	PubSub    redisclient.PubSub
	Locations *LocationTracker
	ReplicaID string

	// RelayTimeout bounds how long a relayed dispatch waits for the owning
	// replica to even acknowledge receipt via its first message; <=0 uses
	// DefaultRelayTimeout. It does not bound the dispatch's own total
	// duration -- that is governed by ctx, exactly as a local Dispatch is.
	RelayTimeout time.Duration

	// relayMu guards relayed, the cancel funcs for dispatches this
	// replica is currently serving on another replica's behalf (Section
	// 8's inFlight, for the relayed-request side).
	relayMu sync.Mutex
	relayed map[string]context.CancelFunc

	// Ready, if set, is closed once ServeRelayedDispatches has
	// successfully subscribed to this replica's request channel and is
	// actively listening. Subscribing happens asynchronously relative to
	// ServeRelayedDispatches typically being started in its own
	// goroutine, so a caller that needs to know listening has actually
	// begun (a readiness probe, or a test coordinating two simulated
	// replicas) should set Ready before calling ServeRelayedDispatches
	// and wait on it rather than sleeping a guessed duration.
	Ready chan struct{}
}

// DefaultRelayTimeout bounds the wait for the owning replica to pick up a
// relayed request at all (distinct from the dispatch's own execution
// time, which can run up to the tool's normal timeout).
const DefaultRelayTimeout = 10 * time.Second

func (b *ReplicaBridge) relayTimeout() time.Duration {
	if b.RelayTimeout > 0 {
		return b.RelayTimeout
	}
	return DefaultRelayTimeout
}

// Dispatch implements the same contract as Bridge.Dispatch (Section 8):
// devices connected locally are served by b.Local with no relay overhead;
// devices connected to a different replica are routed there via PubSub.
func (b *ReplicaBridge) Dispatch(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress ProgressFunc) (protocol.ResultPayload, error) {
	if _, ok := b.Hub.Connection(deviceID); ok {
		return b.Local.Dispatch(ctx, deviceID, correlationID, tool, sessionID, input, onProgress)
	}

	owner, ok := b.Locations.Owner(ctx, deviceID)
	if !ok {
		return protocol.ResultPayload{}, ErrDeviceOffline
	}
	if owner == b.ReplicaID {
		// Our own location record but no local connection: stale (a
		// disconnect raced this lookup). Treat as offline rather than
		// relaying a request to ourselves that can never be served.
		return protocol.ResultPayload{}, ErrDeviceOffline
	}

	return b.dispatchRemote(ctx, owner, deviceID, correlationID, tool, sessionID, input, onProgress)
}

func (b *ReplicaBridge) dispatchRemote(ctx context.Context, owner, deviceID, correlationID, tool, sessionID string, input any, onProgress ProgressFunc) (protocol.ResultPayload, error) {
	start := time.Now()
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return protocol.ResultPayload{}, fmt.Errorf("agent: marshal relay input: %w", err)
	}

	sub, err := b.PubSub.Subscribe(ctx, replyChannel(correlationID))
	if err != nil {
		return protocol.ResultPayload{}, fmt.Errorf("agent: subscribe relay reply: %w", err)
	}
	defer sub.Close()

	req := relayRequest{DeviceID: deviceID, CorrelationID: correlationID, Tool: tool, SessionID: sessionID, Input: inputJSON}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return protocol.ResultPayload{}, fmt.Errorf("agent: marshal relay request: %w", err)
	}
	if err := b.PubSub.Publish(ctx, requestChannel(owner), reqJSON); err != nil {
		return protocol.ResultPayload{}, fmt.Errorf("agent: publish relay request: %w", err)
	}

	result, err := b.waitForRelayResult(ctx, correlationID, owner, sub, onProgress)
	log.Printf("agent: relayed dispatch %s (tool=%s, replica %s -> %s) took %s", correlationID, tool, b.ReplicaID, owner, time.Since(start))
	return result, err
}

func (b *ReplicaBridge) waitForRelayResult(ctx context.Context, correlationID, owner string, sub redisclient.Subscription, onProgress ProgressFunc) (protocol.ResultPayload, error) {
	// The owning replica might be gone/unreachable and never pick up the
	// request at all (unlike a local Dispatch, there is no WS-level
	// failure to detect that). relayTimeout bounds only the wait for the
	// *first* message; once the owner has responded at all, the dispatch
	// is genuinely in flight and is bounded by ctx from then on, exactly
	// like a local Dispatch (so a legitimately long-running tool, e.g.
	// screenshot_watch, is never cut short by this).
	firstMessageTimeout := time.NewTimer(b.relayTimeout())
	defer firstMessageTimeout.Stop()
	gotFirstMessage := false

	cancelled := false
	for {
		select {
		case <-firstMessageTimeout.C:
			if gotFirstMessage {
				continue // already stopped/drained in the normal case; defensive only
			}
			b.publishCancel(correlationID, owner)
			return protocol.ResultPayload{}, fmt.Errorf("agent: replica %s did not respond to relayed dispatch within %s", owner, b.relayTimeout())
		case <-ctx.Done():
			if cancelled {
				return protocol.ResultPayload{}, ctx.Err()
			}
			cancelled = true
			b.publishCancel(correlationID, owner)
			select {
			case raw, chOpen := <-sub.Messages():
				if r, done, ok := b.handleRelayMessage(raw, chOpen, onProgress); done {
					return r, ok
				}
				return protocol.ResultPayload{}, ctx.Err()
			case <-time.After(2 * time.Second):
				return protocol.ResultPayload{}, ctx.Err()
			}
		case raw, chOpen := <-sub.Messages():
			if !gotFirstMessage {
				gotFirstMessage = true
				firstMessageTimeout.Stop()
			}
			if r, done, resErr := b.handleRelayMessage(raw, chOpen, onProgress); done {
				return r, resErr
			}
		}
	}
}

func (b *ReplicaBridge) publishCancel(correlationID, owner string) {
	data, err := json.Marshal(relayCancel{Cancel: true, CorrelationID: correlationID})
	if err != nil {
		return
	}
	_ = b.PubSub.Publish(context.Background(), requestChannel(owner), data)
}

func (b *ReplicaBridge) handleRelayMessage(raw []byte, chOpen bool, onProgress ProgressFunc) (protocol.ResultPayload, bool, error) {
	if !chOpen {
		return protocol.ResultPayload{}, true, ErrConnectionClosed
	}
	var msg relayMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return protocol.ResultPayload{}, false, nil
	}
	if msg.Err != "" {
		return protocol.ResultPayload{}, true, errors.New(msg.Err)
	}
	if msg.Result != nil {
		return *msg.Result, true, nil
	}
	if onProgress != nil {
		if msg.Progress != nil {
			onProgress(msg.Progress, nil)
		} else if msg.BinaryHeader != nil {
			onProgress(nil, &BinaryFrame{Header: *msg.BinaryHeader, Data: msg.BinaryData})
		}
	}
	return protocol.ResultPayload{}, false, nil
}

// ServeRelayedDispatches subscribes to this replica's own request channel
// and, for each relayRequest received, performs the dispatch via b.Local
// (this replica does hold the target agent's connection -- that is why
// another replica routed the request here) and relays progress/binary/
// result messages back over the correlation-scoped reply channel. It
// blocks until ctx is cancelled. If b.Ready is set, it is closed once the
// subscription is active (see Ready's doc comment).
func (b *ReplicaBridge) ServeRelayedDispatches(ctx context.Context) error {
	sub, err := b.PubSub.Subscribe(ctx, requestChannel(b.ReplicaID))
	if err != nil {
		return fmt.Errorf("agent: subscribe relay requests: %w", err)
	}
	if b.Ready != nil {
		close(b.Ready)
	}
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return nil
		case raw, ok := <-sub.Messages():
			if !ok {
				return nil
			}
			b.handleIncomingRelay(ctx, raw)
		}
	}
}

func (b *ReplicaBridge) handleIncomingRelay(ctx context.Context, raw []byte) {
	var cancel relayCancel
	if err := json.Unmarshal(raw, &cancel); err == nil && cancel.Cancel {
		b.cancelRelayed(cancel.CorrelationID)
		return
	}

	var req relayRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.CorrelationID == "" {
		return
	}
	go b.serveOneRelayedDispatch(ctx, req)
}

func (b *ReplicaBridge) registerRelayed(correlationID string, cancel context.CancelFunc) {
	b.relayMu.Lock()
	defer b.relayMu.Unlock()
	if b.relayed == nil {
		b.relayed = map[string]context.CancelFunc{}
	}
	b.relayed[correlationID] = cancel
}

func (b *ReplicaBridge) unregisterRelayed(correlationID string) {
	b.relayMu.Lock()
	defer b.relayMu.Unlock()
	delete(b.relayed, correlationID)
}

func (b *ReplicaBridge) cancelRelayed(correlationID string) {
	b.relayMu.Lock()
	cancel, ok := b.relayed[correlationID]
	b.relayMu.Unlock()
	if ok {
		cancel()
	}
}

func (b *ReplicaBridge) serveOneRelayedDispatch(ctx context.Context, req relayRequest) {
	dispatchCtx, cancel := context.WithCancel(ctx)
	b.registerRelayed(req.CorrelationID, cancel)
	defer func() {
		cancel()
		b.unregisterRelayed(req.CorrelationID)
	}()

	onProgress := func(payload *protocol.ProgressPayload, binary *BinaryFrame) {
		msg := relayMessage{Progress: payload}
		if binary != nil {
			h := binary.Header
			msg = relayMessage{BinaryHeader: &h, BinaryData: binary.Data}
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return
		}
		_ = b.PubSub.Publish(context.Background(), replyChannel(req.CorrelationID), data)
	}

	var input any
	_ = json.Unmarshal(req.Input, &input)
	result, err := b.Local.Dispatch(dispatchCtx, req.DeviceID, req.CorrelationID, req.Tool, req.SessionID, input, onProgress)

	var out relayMessage
	if err != nil {
		out = relayMessage{Err: err.Error()}
	} else {
		out = relayMessage{Result: &result}
	}
	data, mErr := json.Marshal(out)
	if mErr != nil {
		return
	}
	_ = b.PubSub.Publish(context.Background(), replyChannel(req.CorrelationID), data)
}

// RunLocationHeartbeat periodically refreshes this replica's location
// records for every currently-connected device, so a long-lived
// connection's record doesn't expire between connect/disconnect events.
// Blocks until ctx is cancelled.
func (b *ReplicaBridge) RunLocationHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = b.Locations.ttl() / 3
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.Locations.Refresh(ctx, b.Hub.OnlineDeviceIDs())
		}
	}
}
