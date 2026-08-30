// Package client implements the desktop agent's WebSocket connection to
// rc-mcp-server: dialing with exponential backoff, the hello/hello_ack
// re-authentication handshake on reconnect, and a keepalive heartbeat. See
// docs/specs/backend.md Section 2.1.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/CloudKeter/rc-mcp/internal/protocol"
)

const (
	backoffMin = 1 * time.Second
	backoffMax = 30 * time.Second
	// jitterFraction is the +/-20% jitter applied to each backoff delay.
	jitterFraction = 0.20

	writeWait = 10 * time.Second
)

// pingPeriod is the agent-initiated keepalive interval, and pongTimeout is
// how long the agent waits for a pong after a ping before deciding the
// connection is dead and reconnecting (Section 2.1: 30s / 10s). Declared
// as vars, not consts, so tests can shrink them instead of waiting 30s
// real time for heartbeat behavior.
var (
	pingPeriod  = 30 * time.Second
	pongTimeout = 10 * time.Second
)

// NextBackoff returns the delay before reconnect attempt number `attempt`
// (1-indexed: the delay before the *first* retry after an initial failed
// attempt), following the 1s, 2s, 4s, 8s, 16s, 30s (capped) schedule with
// +/-20% jitter. attempt <= 1 returns the first step.
func NextBackoff(attempt int) time.Duration {
	return nextBackoffWithRand(attempt, rand.Float64)
}

// nextBackoffWithRand is NextBackoff with an injectable source of
// randomness in [0,1) for deterministic tests.
func nextBackoffWithRand(attempt int, randFloat64 func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := backoffMin << (attempt - 1) // 1,2,4,8,16,32,64,...
	if base > backoffMax || base <= 0 {
		base = backoffMax
	}

	jitter := (randFloat64()*2 - 1) * jitterFraction // in [-0.2, 0.2)
	d := time.Duration(float64(base) * (1 + jitter))
	if d < 0 {
		d = 0
	}
	return d
}

// Client dials rc-mcp-server's agent WebSocket endpoint.
type Client struct {
	ServerURL string
	Dialer    *websocket.Dialer
}

// NewClient constructs a Client for the given server URL (e.g.
// "wss://server-host/agent/ws").
func NewClient(serverURL string) *Client {
	return &Client{ServerURL: serverURL, Dialer: websocket.DefaultDialer}
}

// DialRaw performs the WebSocket upgrade with no hello/pairing exchange.
// Used by the pairing flow (agent/client/pairing.go), which sends
// pair_request itself.
func (c *Client) DialRaw(ctx context.Context) (*websocket.Conn, error) {
	dialer := c.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, _, err := dialer.DialContext(ctx, c.ServerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.ServerURL, err)
	}
	return conn, nil
}

// DialWithBackoff calls DialRaw repeatedly, sleeping NextBackoff(attempt)
// between failures, until it succeeds or ctx is cancelled.
func (c *Client) DialWithBackoff(ctx context.Context) (*websocket.Conn, error) {
	attempt := 0
	for {
		conn, err := c.DialRaw(ctx)
		if err == nil {
			return conn, nil
		}
		attempt++
		delay := NextBackoff(attempt)
		log.Printf("agent: dial failed (attempt %d): %v; retrying in %s", attempt, err, delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Connect dials the server (with backoff) and performs the hello/hello_ack
// handshake using the persisted device token, i.e. a reconnect that
// requires no re-pairing.
func (c *Client) Connect(ctx context.Context, token, hostname string, capabilities []string) (*Connection, *protocol.HelloAckPayload, error) {
	conn, err := c.DialWithBackoff(ctx)
	if err != nil {
		return nil, nil, err
	}

	if err := conn.WriteJSON(protocol.Envelope{
		Type:            protocol.MsgHello,
		ProtocolVersion: protocol.Version,
		Ts:              time.Now().UTC(),
		Payload: protocol.HelloPayload{
			DeviceToken:  token,
			Hostname:     hostname,
			Capabilities: capabilities,
		},
	}); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("send hello: %w", err)
	}

	var ack protocol.Envelope
	if err := conn.ReadJSON(&ack); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read hello response: %w", err)
	}
	if ack.Type == protocol.MsgError {
		conn.Close()
		payload, _ := decodePayload[protocol.ErrorPayload](ack.Payload)
		return nil, nil, fmt.Errorf("server rejected hello: %s: %s", payload.Code, payload.Message)
	}
	if ack.Type != protocol.MsgHelloAck {
		conn.Close()
		return nil, nil, fmt.Errorf("expected hello_ack, got %s", ack.Type)
	}
	helloAck, err := decodePayload[protocol.HelloAckPayload](ack.Payload)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("decode hello_ack: %w", err)
	}

	return newConnection(conn), &helloAck, nil
}

// Connection wraps an established WebSocket with a heartbeat. Dead()
// closes when the heartbeat detects a missed pong or a write/read fails,
// signaling the caller to reconnect.
type Connection struct {
	ws *websocket.Conn

	mu sync.Mutex

	dead     chan struct{}
	deadOnce sync.Once
}

func newConnection(ws *websocket.Conn) *Connection {
	c := &Connection{ws: ws, dead: make(chan struct{})}
	ws.SetPongHandler(func(string) error {
		return nil
	})
	go c.heartbeatLoop()
	return c
}

// Dead closes when this connection should no longer be used and the
// caller should reconnect.
func (c *Connection) Dead() <-chan struct{} { return c.dead }

func (c *Connection) markDead() {
	c.deadOnce.Do(func() { close(c.dead) })
}

func (c *Connection) heartbeatLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			pongCh := make(chan struct{}, 1)
			c.ws.SetPongHandler(func(string) error {
				select {
				case pongCh <- struct{}{}:
				default:
				}
				return nil
			})

			c.mu.Lock()
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
			c.mu.Unlock()
			if err != nil {
				c.markDead()
				return
			}

			select {
			case <-pongCh:
				// alive
			case <-time.After(pongTimeout):
				log.Printf("agent: no pong within %s, reconnecting", pongTimeout)
				c.markDead()
				return
			case <-c.dead:
				return
			}
		case <-c.dead:
			return
		}
	}
}

// SendEnvelope writes an envelope to the connection. Safe for concurrent
// use with the heartbeat's ping control frames (gorilla/websocket allows
// WriteControl concurrently with WriteMessage), but not with other
// SendEnvelope calls -- serialize writes at a higher level if needed.
func (c *Connection) SendEnvelope(env protocol.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err := c.ws.WriteJSON(env); err != nil {
		c.markDead()
		return err
	}
	return nil
}

// SendBinary writes a raw binary WebSocket frame (a BinaryHeader-prefixed
// shell stdout chunk, screenshot frame, etc.) to the connection. Safe for
// concurrent use with SendEnvelope (both serialize through c.mu).
func (c *Connection) SendBinary(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err := c.ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
		c.markDead()
		return err
	}
	return nil
}

// ReadEnvelope blocks until the next JSON envelope arrives or the
// connection fails, in which case it marks the connection dead.
func (c *Connection) ReadEnvelope() (protocol.Envelope, error) {
	var env protocol.Envelope
	if err := c.ws.ReadJSON(&env); err != nil {
		c.markDead()
		return env, err
	}
	return env, nil
}

// Close closes the underlying WebSocket connection.
func (c *Connection) Close() error {
	c.markDead()
	return c.ws.Close()
}

// decodePayload converts an Envelope's `any`-typed Payload (already
// json.Unmarshal'd into a map[string]any by the standard decoder) into a
// concrete payload struct.
func decodePayload[T any](payload any) (T, error) {
	var out T
	raw, err := json.Marshal(payload)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}
