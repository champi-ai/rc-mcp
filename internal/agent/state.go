package agent

import (
	"fmt"
	"sync"
	"time"

	"github.com/champi-ai/rc-mcp/internal/protocol"
)

// DefaultReconnectGracePeriod is the AGENT_RECONNECT_GRACE_PERIOD default
// (Section 2.1: the server holds in-flight dispatch state for 60s after an
// agent disconnect; reconnecting within the window resumes streaming).
const DefaultReconnectGracePeriod = 60 * time.Second

// deviceState is the per-device dispatch state owned by the Hub. It holds
// the pending correlation registrations that route progress/result/error
// envelopes and binary frames back to waiting dispatch bridges. Unlike a
// Connection, a deviceState survives a reconnect: while a disconnect is
// within the grace period, registrations stay open so a resumed agent's
// frames reach the original bridges (and their MCP sessions).
type deviceState struct {
	mu         sync.Mutex
	pending    map[string]chan bridgeMessage // full correlation ID -> channel
	byPrefix   map[[4]byte]chan bridgeMessage
	graceTimer *time.Timer // running while disconnected with pending work
}

func newDeviceState() *deviceState {
	return &deviceState{
		pending:  map[string]chan bridgeMessage{},
		byPrefix: map[[4]byte]chan bridgeMessage{},
	}
}

// registerPending associates correlationID with a fresh, buffered channel
// that will receive every progress/result/error envelope and binary frame
// addressed to it. Callers must call unregisterPending(correlationID) once
// done, in all cases, to avoid leaking the registration.
func (s *deviceState) registerPending(correlationID string) (chan bridgeMessage, error) {
	prefix, err := protocol.CorrelationPrefix(correlationID)
	if err != nil {
		return nil, err
	}

	ch := make(chan bridgeMessage, 64)
	s.mu.Lock()
	if _, collision := s.byPrefix[prefix]; collision {
		s.mu.Unlock()
		return nil, fmt.Errorf("agent connection: binary correlation prefix collision for %s", correlationID)
	}
	s.pending[correlationID] = ch
	s.byPrefix[prefix] = ch
	s.mu.Unlock()
	return ch, nil
}

func (s *deviceState) unregisterPending(correlationID string) {
	prefix, err := protocol.CorrelationPrefix(correlationID)
	s.mu.Lock()
	delete(s.pending, correlationID)
	if err == nil {
		delete(s.byPrefix, prefix)
	}
	s.mu.Unlock()
}

func (s *deviceState) route(correlationID string) (chan bridgeMessage, bool) {
	s.mu.Lock()
	ch, ok := s.pending[correlationID]
	s.mu.Unlock()
	return ch, ok
}

func (s *deviceState) routePrefix(prefix [4]byte) (chan bridgeMessage, bool) {
	s.mu.Lock()
	ch, ok := s.byPrefix[prefix]
	s.mu.Unlock()
	return ch, ok
}

// hasPending reports whether any dispatch is still in flight.
func (s *deviceState) hasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) > 0
}

// startGrace arms the grace timer: if not resumed (or expired directly)
// within d, every pending registration's channel is closed, which the
// waiting bridges observe as ErrConnectionClosed.
func (s *deviceState) startGrace(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.graceTimer != nil {
		s.graceTimer.Stop()
	}
	s.graceTimer = time.AfterFunc(d, s.expire)
}

// resume cancels a running grace timer. It returns true if there was
// in-flight work being held for the reconnecting agent (Section 2.1:
// hello_ack resume flag).
func (s *deviceState) resume() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	return len(s.pending) > 0
}

// expire closes every pending channel (bridges see ErrConnectionClosed)
// and clears the registrations. Called by the grace timer, or directly on
// revocation.
func (s *deviceState) expire() {
	s.mu.Lock()
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	chans := make([]chan bridgeMessage, 0, len(s.pending))
	for _, ch := range s.pending {
		chans = append(chans, ch)
	}
	s.pending = map[string]chan bridgeMessage{}
	s.byPrefix = map[[4]byte]chan bridgeMessage{}
	s.mu.Unlock()

	for _, ch := range chans {
		close(ch)
	}
}
