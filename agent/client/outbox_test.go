package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/CloudKeter/rc-mcp/internal/protocol"
)

// newSinkServer upgrades one WS connection and streams every received
// frame into the returned channels.
func newSinkServer(t *testing.T) (*httptest.Server, chan protocol.Envelope, chan []byte) {
	t.Helper()
	envCh := make(chan protocol.Envelope, 512)
	binCh := make(chan []byte, 512)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.BinaryMessage {
				binCh <- data
				continue
			}
			var env protocol.Envelope
			if err := json.Unmarshal(data, &env); err == nil {
				envCh <- env
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, envCh, binCh
}

func dialConn(t *testing.T, srv *httptest.Server) *Connection {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn := newConnection(ws)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func env(id string) protocol.Envelope {
	return protocol.Envelope{Type: protocol.MsgResult, ID: id, Ts: time.Now().UTC()}
}

func TestOutbox_BuffersWhileDetachedAndFlushesOnResume(t *testing.T) {
	srv, envCh, binCh := newSinkServer(t)
	o := NewOutbox()

	// Detached: everything buffers, sends never fail.
	if err := o.SendBinary([]byte("frame-1")); err != nil {
		t.Fatalf("SendBinary: %v", err)
	}
	if err := o.SendEnvelope(env("result-1")); err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}

	// Resume: frames flush before envelopes.
	o.Attach(dialConn(t, srv), true)

	select {
	case data := <-binCh:
		if string(data) != "frame-1" {
			t.Fatalf("frame = %q", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("buffered frame not flushed")
	}
	select {
	case e := <-envCh:
		if e.ID != "result-1" {
			t.Fatalf("envelope = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("buffered envelope not flushed")
	}

	// Attached: writes pass straight through.
	_ = o.SendEnvelope(env("result-2"))
	select {
	case e := <-envCh:
		if e.ID != "result-2" {
			t.Fatalf("envelope = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live envelope not delivered")
	}
}

func TestOutbox_FreshAttachDropsBacklog(t *testing.T) {
	srv, envCh, binCh := newSinkServer(t)
	o := NewOutbox()
	_ = o.SendBinary([]byte("stale-frame"))
	_ = o.SendEnvelope(env("stale-result"))

	o.Attach(dialConn(t, srv), false)
	_ = o.SendEnvelope(env("fresh"))

	select {
	case e := <-envCh:
		if e.ID != "fresh" {
			t.Fatalf("stale backlog was flushed: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live envelope not delivered")
	}
	select {
	case data := <-binCh:
		t.Fatalf("stale frame was flushed: %q", data)
	default:
	}
}

func TestOutbox_DropClearsBacklog(t *testing.T) {
	srv, envCh, _ := newSinkServer(t)
	o := NewOutbox()
	_ = o.SendEnvelope(env("orphaned"))
	o.Drop()
	o.Attach(dialConn(t, srv), true)

	select {
	case e := <-envCh:
		t.Fatalf("dropped backlog was flushed: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestOutbox_BoundedBuffersDropOldest(t *testing.T) {
	o := NewOutbox()
	for i := 0; i < maxBufferedEnvelopes+10; i++ {
		_ = o.SendEnvelope(env(fmt.Sprintf("e-%d", i)))
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.envelopes) != maxBufferedEnvelopes {
		t.Fatalf("len = %d, want %d", len(o.envelopes), maxBufferedEnvelopes)
	}
	if o.envelopes[0].ID != "e-10" {
		t.Fatalf("oldest kept = %s, want e-10 (oldest dropped first)", o.envelopes[0].ID)
	}
}
