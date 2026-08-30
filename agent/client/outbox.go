package client

import (
	"log"
	"sync"

	"github.com/champi-ai/rc-mcp/internal/protocol"
)

const (
	// maxBufferedEnvelopes bounds how many JSON envelopes (results,
	// progress) the outbox holds while disconnected.
	maxBufferedEnvelopes = 256
	// maxBufferedFrames bounds how many binary frames (screenshot PNGs,
	// PTY output chunks) the outbox holds while disconnected. Oldest
	// frames are dropped first -- for a screenshot watch the newest frames
	// are the valuable ones.
	maxBufferedFrames = 128
)

// Outbox is the send path in-flight dispatches use instead of writing to a
// Connection directly. While connected it forwards writes; while
// disconnected it buffers them (bounded, oldest dropped first) so a
// reconnect within the server's grace period (hello_ack resume:true) can
// flush everything the server missed (Section 2.1, "In-flight job state
// across brief disconnects").
type Outbox struct {
	mu        sync.Mutex
	conn      *Connection
	envelopes []protocol.Envelope
	frames    [][]byte
}

// NewOutbox constructs a detached Outbox.
func NewOutbox() *Outbox {
	return &Outbox{}
}

// Attach makes conn the active send path. With flush true (a resume), the
// buffered backlog is sent first -- binary frames, then envelopes, so a
// dispatch's terminal result never precedes its own streamed frames. With
// flush false (a fresh session server-side), the backlog is dropped.
func (o *Outbox) Attach(conn *Connection, flush bool) {
	o.mu.Lock()
	envelopes, frames := o.envelopes, o.frames
	o.envelopes, o.frames = nil, nil
	o.conn = conn
	o.mu.Unlock()

	if !flush {
		return
	}
	for _, data := range frames {
		if err := conn.SendBinary(data); err != nil {
			log.Printf("agent outbox: flush failed: %v", err)
			return
		}
	}
	for _, env := range envelopes {
		if err := conn.SendEnvelope(env); err != nil {
			log.Printf("agent outbox: flush failed: %v", err)
			return
		}
	}
	if len(frames) > 0 || len(envelopes) > 0 {
		log.Printf("agent outbox: flushed %d frames and %d envelopes after resume", len(frames), len(envelopes))
	}
}

// Detach removes the active connection; subsequent sends buffer.
func (o *Outbox) Detach() {
	o.mu.Lock()
	o.conn = nil
	o.mu.Unlock()
}

// Drop discards the buffered backlog (grace period expired; the server
// has already failed the corresponding dispatches).
func (o *Outbox) Drop() {
	o.mu.Lock()
	o.envelopes, o.frames = nil, nil
	o.mu.Unlock()
}

// SendEnvelope forwards env to the active connection, or buffers it while
// disconnected. It never fails the caller: a dispatch's outcome is either
// delivered, held for a resume, or dropped when the grace period lapses.
func (o *Outbox) SendEnvelope(env protocol.Envelope) error {
	o.mu.Lock()
	conn := o.conn
	o.mu.Unlock()

	if conn != nil {
		if err := conn.SendEnvelope(env); err == nil {
			return nil
		}
		// Fall through to buffer: the connection just died.
	}

	o.mu.Lock()
	if len(o.envelopes) >= maxBufferedEnvelopes {
		o.envelopes = o.envelopes[1:]
	}
	o.envelopes = append(o.envelopes, env)
	o.mu.Unlock()
	return nil
}

// SendBinary forwards a raw binary frame, or buffers it while disconnected.
func (o *Outbox) SendBinary(data []byte) error {
	o.mu.Lock()
	conn := o.conn
	o.mu.Unlock()

	if conn != nil {
		if err := conn.SendBinary(data); err == nil {
			return nil
		}
	}

	o.mu.Lock()
	if len(o.frames) >= maxBufferedFrames {
		o.frames = o.frames[1:]
	}
	o.frames = append(o.frames, data)
	o.mu.Unlock()
	return nil
}
