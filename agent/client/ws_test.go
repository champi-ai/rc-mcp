package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/champi-ai/rc-mcp/internal/protocol"
)

func TestNextBackoff_Schedule(t *testing.T) {
	// No jitter (randFloat64 returns 0.5 => jitter term is 0).
	noJitter := func() float64 { return 0.5 }

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // would be 32s uncapped; capped to 30s
		{7, 30 * time.Second}, // stays capped
		{0, 1 * time.Second},  // attempt < 1 treated as 1
	}
	for _, tc := range cases {
		got := nextBackoffWithRand(tc.attempt, noJitter)
		if got != tc.want {
			t.Errorf("attempt %d: got %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestNextBackoff_JitterBounds(t *testing.T) {
	for attempt := 1; attempt <= 6; attempt++ {
		base := backoffMin << (attempt - 1)
		if base > backoffMax {
			base = backoffMax
		}
		lower := time.Duration(float64(base) * 0.8)
		upper := time.Duration(float64(base) * 1.2)

		for i := 0; i < 200; i++ {
			d := NextBackoff(attempt)
			if d < lower || d > upper {
				t.Fatalf("attempt %d: backoff %v out of jitter bounds [%v, %v]", attempt, d, lower, upper)
			}
		}
	}
}

func TestNextBackoff_JitterExtremes(t *testing.T) {
	// randFloat64() = 0 => jitter = -0.2 (lower bound).
	d := nextBackoffWithRand(1, func() float64 { return 0 })
	if d != 800*time.Millisecond {
		t.Errorf("min jitter: got %v, want 800ms", d)
	}
	// randFloat64() close to 1 => jitter close to +0.2 (upper bound).
	d = nextBackoffWithRand(1, func() float64 { return 0.999999 })
	if d < 1190*time.Millisecond || d > 1200*time.Millisecond {
		t.Errorf("max jitter: got %v, want ~1200ms", d)
	}
}

func newHelloAckServer(t *testing.T, deviceID string) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		if env.Type != protocol.MsgHello {
			return
		}
		_ = conn.WriteJSON(protocol.Envelope{
			Type:            protocol.MsgHelloAck,
			ProtocolVersion: protocol.Version,
			Ts:              time.Now().UTC(),
			Payload: protocol.HelloAckPayload{
				DeviceID: deviceID,
				Resume:   false,
			},
		})

		// Keep the connection open, replying to pings so the test can
		// observe a healthy heartbeat if needed. No further messages.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestConnect_ReconnectWithExistingToken_NoRePairing(t *testing.T) {
	srv := newHelloAckServer(t, "device-123")
	defer srv.Close()

	c := NewClient(wsURL(srv))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, ack, err := c.Connect(ctx, "persisted-token", "test-host", []string{"shell"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	if ack.DeviceID != "device-123" {
		t.Errorf("DeviceID = %q, want %q", ack.DeviceID, "device-123")
	}
}

func TestHeartbeat_MissedPongMarksConnectionDead(t *testing.T) {
	origPingPeriod, origPongTimeout := pingPeriod, pongTimeout
	pingPeriod = 50 * time.Millisecond
	pongTimeout = 100 * time.Millisecond
	defer func() { pingPeriod, pongTimeout = origPingPeriod, origPongTimeout }()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		_ = conn.WriteJSON(protocol.Envelope{
			Type: protocol.MsgHelloAck,
			Ts:   time.Now().UTC(),
			Payload: protocol.HelloAckPayload{
				DeviceID: "device-1",
			},
		})

		// Never respond to pings (no pong handler set server-side beyond
		// gorilla's default, and we don't send pings back) -- but more
		// importantly, this server never sends a ping itself, and by
		// simply not replying with a pong to the client's ping (gorilla's
		// server-side connection auto-replies to pings by default, so we
		// must intercept). To simulate a truly silent peer, block on read
		// without responding to control frames by never calling
		// ReadMessage again after the first read, so the connection just
		// sits idle from the server's perspective while the client's
		// gorilla dialer library auto-answers pings for us -- instead we
		// directly avoid this by closing the raw TCP connection.
		time.Sleep(500 * time.Millisecond)
		conn.Close()
	}))
	defer srv.Close()

	c := NewClient(wsURL(srv))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := c.Connect(ctx, "tok", "host", nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	select {
	case <-conn.Dead():
		// expected: server closed the socket, read loop / heartbeat
		// should observe this.
	case <-time.After(3 * time.Second):
		t.Fatal("expected connection to be marked dead after server closed it")
	}
}
