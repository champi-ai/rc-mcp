package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/champi-ai/rc-mcp/internal/protocol"
)

const (
	// pingPeriod is how often the server sends a WebSocket ping control
	// frame to a connected agent (Section 2.1: "server-initiated ping
	// every 30s").
	pingPeriod = 30 * time.Second

	// pongWait is the deadline after which, without receiving a pong (or
	// any other frame), the connection is considered dead. Set to
	// pingPeriod + the 10s pong timeout from Section 2.1.
	pongWait = pingPeriod + 10*time.Second

	// writeWait bounds how long a single write may take.
	writeWait = 10 * time.Second

	sendBufferSize = 32
)

// Connection is a single agent's WebSocket connection: one reader loop and
// one writer goroutine (all outbound writes go through the writer to avoid
// concurrent WebSocket writes), plus a heartbeat.
type Connection struct {
	hub *Hub
	ws  *websocket.Conn

	send chan protocol.Envelope

	deviceID string // set once authenticated

	closeOnce sync.Once
	done      chan struct{}

	// stopWriter tells the writer goroutine to drain what is buffered and
	// exit. c.send is never closed (trySend may race teardown, e.g. from
	// Hub.Shutdown or RevokeDevice, and sending on a closed channel would
	// panic).
	stopWriter chan struct{}
}

// bridgeMessage is either a decoded JSON envelope or a binary frame,
// delivered to whichever dispatch bridge registered the matching
// correlation ID/prefix.
type bridgeMessage struct {
	Envelope *protocol.Envelope
	Header   protocol.BinaryHeader
	Data     []byte
}

func newConnection(h *Hub, ws *websocket.Conn) *Connection {
	return &Connection{
		hub:        h,
		ws:         ws,
		send:       make(chan protocol.Envelope, sendBufferSize),
		done:       make(chan struct{}),
		stopWriter: make(chan struct{}),
	}
}

// registerPending associates correlationID with a fresh, buffered channel
// on this connection's device state (owned by the Hub, so it survives a
// reconnect within the grace period). Callers must call
// unregisterPending(correlationID) once done, in all cases (including
// error paths), to avoid leaking the registration.
func (c *Connection) registerPending(correlationID string) (chan bridgeMessage, error) {
	return c.hub.state(c.deviceID).registerPending(correlationID)
}

func (c *Connection) unregisterPending(correlationID string) {
	c.hub.state(c.deviceID).unregisterPending(correlationID)
}

// routeEnvelope delivers a progress/result/error envelope to the bridge
// waiting on its correlation ID, if any. Returns false if nothing is
// registered for that ID (e.g. it already completed or was never ours).
func (c *Connection) routeEnvelope(env protocol.Envelope) bool {
	ch, ok := c.hub.state(c.deviceID).route(env.ID)
	if !ok {
		return false
	}
	select {
	case ch <- bridgeMessage{Envelope: &env}:
	default:
		log.Printf("agent connection: dropping %s for %s: bridge channel full", env.Type, env.ID)
	}
	return true
}

// routeBinaryFrame demuxes a binary WS frame by its header's correlation
// prefix and delivers it to the matching bridge, if any.
func (c *Connection) routeBinaryFrame(data []byte) bool {
	if len(data) < protocol.BinaryHeaderSize {
		return false
	}
	h := protocol.DecodeBinaryHeader(data)
	payload := append([]byte(nil), data[protocol.BinaryHeaderSize:]...)

	ch, ok := c.hub.state(c.deviceID).routePrefix(h.CorrelationPrefix)
	if !ok {
		return false
	}
	select {
	case ch <- bridgeMessage{Header: h, Data: payload}:
	default:
		log.Printf("agent connection: dropping binary frame type %v: bridge channel full", h.FrameType)
	}
	return true
}

// run drives the connection: starts the writer and heartbeat goroutines,
// performs the hello/pairing handshake, then reads messages until the
// connection closes. It blocks until the connection is fully torn down.
func (c *Connection) run() {
	defer c.teardown()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writeLoop()
	}()

	c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		c.ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		c.heartbeatLoop()
	}()

	if err := c.handshake(); err != nil {
		log.Printf("agent connection: handshake failed: %v", err)
		close(c.stopWriter)
		wg.Wait() // ensure any queued error envelope is flushed before closing
		code, reason := closeCodeForHandshakeError(err)
		c.closeWithReason(code, reason)
		<-heartbeatDone
		return
	}

	c.readLoop()

	close(c.stopWriter)
	wg.Wait()
	// The read loop exiting means the socket is gone (or closing): close
	// c.done now so the heartbeat loop stops immediately instead of
	// waiting out its ping interval -- teardown (and with it the reconnect
	// grace timer, Section 2.1) must start promptly after a disconnect.
	c.closeWithReason(websocket.CloseNormalClosure, "connection_closed")
	<-heartbeatDone
}

// closeCodeForHandshakeError maps a handshake failure to the WebSocket
// close code and reason to send, per docs/specs/backend.md Section 2.2 and
// Section 12.2.
func closeCodeForHandshakeError(err error) (int, string) {
	switch {
	case errors.Is(err, errAuthFailed):
		return websocket.ClosePolicyViolation, "auth_failed"
	case errors.Is(err, errVersionMismatch):
		return websocket.CloseProtocolError, "version_mismatch"
	case errors.Is(err, errPairingExpired):
		return websocket.CloseNormalClosure, "pairing_expired"
	case errors.Is(err, errPairingRejected):
		return websocket.CloseNormalClosure, "pairing_rejected"
	case errors.Is(err, errUnexpectedFirstMessage):
		return websocket.CloseProtocolError, "invalid_handshake"
	case errors.Is(err, errConnectionClosed):
		return websocket.CloseNormalClosure, "connection_closed"
	default:
		return websocket.CloseProtocolError, "invalid_payload"
	}
}

func (c *Connection) teardown() {
	if c.deviceID != "" {
		c.hub.unregisterOnline(c.deviceID, c)
		if c.hub.Registry != nil {
			_ = c.hub.Registry.SetOnline(context.Background(), c.deviceID, false)
		}
	}
	c.ws.Close()
}

// closeWithReason sends a WebSocket close frame with the given code/reason
// and stops the connection. Safe to call multiple times or concurrently.
func (c *Connection) closeWithReason(code int, reason string) {
	c.closeOnce.Do(func() {
		msg := websocket.FormatCloseMessage(code, reason)
		_ = c.ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(writeWait))
		close(c.done)
	})
}

// trySend enqueues an envelope for the writer goroutine. It never blocks
// the caller indefinitely: if the send buffer is full the envelope is
// dropped and logged (the connection is presumed unhealthy; the heartbeat
// will eventually close it).
func (c *Connection) trySend(env protocol.Envelope) {
	select {
	case c.send <- env:
	default:
		log.Printf("agent connection: send buffer full, dropping %s message", env.Type)
	}
}

func (c *Connection) writeLoop() {
	write := func(env protocol.Envelope) bool {
		c.ws.SetWriteDeadline(time.Now().Add(writeWait))
		if err := c.ws.WriteJSON(env); err != nil {
			log.Printf("agent connection: write failed: %v", err)
			return false
		}
		return true
	}
	for {
		select {
		case env := <-c.send:
			if !write(env) {
				return
			}
		case <-c.stopWriter:
			// Drain whatever is still buffered (e.g. a queued error or
			// close envelope), then exit.
			for {
				select {
				case env := <-c.send:
					if !write(env) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

func (c *Connection) heartbeatLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// handshake performs the initial hello/pair_request exchange. On success,
// c.deviceID is set and the device is registered online.
func (c *Connection) handshake() error {
	var env protocol.Envelope
	if err := c.ws.ReadJSON(&env); err != nil {
		return err
	}

	switch env.Type {
	case protocol.MsgHello:
		return c.handleHello(env)
	case protocol.MsgPairRequest:
		return c.handlePairRequest(env)
	default:
		c.sendError("invalid_handshake", "expected hello or pair_request as the first message")
		return errUnexpectedFirstMessage
	}
}

func decodePayload[T any](payload any) (T, error) {
	var out T
	raw, err := json.Marshal(payload)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (c *Connection) handleHello(env protocol.Envelope) error {
	if !protocol.IsSupportedVersion(env.ProtocolVersion) {
		c.trySend(protocol.VersionMismatchEnvelope(env.ProtocolVersion))
		return errVersionMismatch
	}

	hello, err := decodePayload[protocol.HelloPayload](env.Payload)
	if err != nil {
		c.sendError("invalid_payload", "malformed hello payload")
		return err
	}

	device, err := c.hub.Registry.Authenticate(context.Background(), hello.DeviceToken)
	if err != nil {
		c.sendError("auth_failed", "device token invalid or revoked")
		return errAuthFailed
	}

	if err := c.hub.Registry.SetOnline(context.Background(), device.ID, true); err != nil {
		log.Printf("agent connection: SetOnline failed: %v", err)
	}
	c.deviceID = device.ID
	resumed := c.hub.registerOnline(device.ID, c)

	c.trySend(protocol.Envelope{
		Type:            protocol.MsgHelloAck,
		ID:              env.ID,
		ProtocolVersion: protocol.Version,
		Ts:              time.Now().UTC(),
		Payload: protocol.HelloAckPayload{
			DeviceID:           device.ID,
			Resume:             resumed,
			LatestAgentVersion: c.hub.LatestAgentVersion,
		},
	})
	return nil
}

func (c *Connection) handlePairRequest(env protocol.Envelope) error {
	req, err := decodePayload[protocol.PairRequestPayload](env.Payload)
	if err != nil {
		c.sendError("invalid_payload", "malformed pair_request payload")
		return err
	}

	pc, err := c.hub.Registry.CreatePairingCode(context.Background(), req.Hostname)
	if err != nil {
		c.sendError("pairing_failed", "failed to generate pairing code")
		return err
	}

	c.trySend(protocol.Envelope{
		Type: protocol.MsgPairCode,
		ID:   env.ID,
		Ts:   time.Now().UTC(),
		Payload: protocol.PairCodePayload{
			Code:      pc.Code,
			ExpiresAt: pc.ExpiresAt,
		},
	})

	ch := c.hub.waitForApproval(pc.Code)
	timer := time.NewTimer(time.Until(pc.ExpiresAt))
	defer timer.Stop()

	select {
	case res := <-ch:
		if !res.approved {
			c.sendError("pairing_rejected", "pairing code was rejected by the operator")
			return errPairingRejected
		}

		c.trySend(protocol.Envelope{
			Type: protocol.MsgPairApproved,
			ID:   env.ID,
			Ts:   time.Now().UTC(),
			Payload: protocol.PairApprovedPayload{
				DeviceID:    res.device.ID,
				DeviceToken: res.token,
			},
		})

		if err := c.hub.Registry.SetOnline(context.Background(), res.device.ID, true); err != nil {
			log.Printf("agent connection: SetOnline failed: %v", err)
		}
		c.deviceID = res.device.ID
		c.hub.registerOnline(res.device.ID, c)
		return nil

	case <-timer.C:
		c.hub.stopWaitingForApproval(pc.Code)
		c.sendError("pairing_expired", "pairing code expired before approval")
		return errPairingExpired

	case <-c.done:
		c.hub.stopWaitingForApproval(pc.Code)
		return errConnectionClosed
	}
}

// readLoop reads subsequent messages after a successful handshake: text
// frames are JSON envelopes (progress/result/error, routed to whichever
// dispatch bridge is waiting on their correlation ID); binary frames are
// demuxed by their header's correlation prefix to the same bridges. See
// docs/specs/backend.md Section 2.2 and Section 8.
func (c *Connection) readLoop() {
	for {
		msgType, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		switch msgType {
		case websocket.TextMessage:
			var env protocol.Envelope
			if err := json.Unmarshal(data, &env); err != nil {
				log.Printf("agent connection: malformed envelope from device %s: %v", c.deviceID, err)
				continue
			}
			switch env.Type {
			case protocol.MsgProgress, protocol.MsgResult, protocol.MsgError:
				c.routeEnvelope(env)
			default:
				// hello/pair_*/ping/pong/close/cancel: not routed to
				// dispatch bridges. Cancel is server->agent only; agents
				// never send it to us.
			}
		case websocket.BinaryMessage:
			c.routeBinaryFrame(data)
		}
	}
}

// sendCloseMessage enqueues a "close" JSON envelope (e.g. reason
// "server_shutdown") ahead of the WebSocket close frame that closeWithReason
// sends. It does not wait for delivery; callers that need delivery
// guarantees should give the writer goroutine a brief moment to flush
// before tearing down the connection.
func (c *Connection) sendCloseMessage(reason string) {
	c.trySend(protocol.Envelope{
		Type: protocol.MsgClose,
		Ts:   time.Now().UTC(),
		Payload: protocol.ClosePayload{
			Reason: reason,
		},
	})
}

func (c *Connection) sendError(code, message string) {
	c.trySend(protocol.Envelope{
		Type: protocol.MsgError,
		Ts:   time.Now().UTC(),
		Payload: protocol.ErrorPayload{
			Code:    code,
			Message: message,
		},
	})
}
