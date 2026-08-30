// Package session implements MCP session state: the per-Mcp-Session-Id
// record of negotiated capabilities, the SSE fan-in event channel and
// replay buffer, active shell session mappings, and pending
// server-initiated request/response correlation (elicitation). See
// docs/specs/backend.md Section 7.
package session

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// EventChBufferSize is the size of Session.EventCh (Section 7/8).
const EventChBufferSize = 256

// SSEEvent is a single event fanned in to Session.EventCh by whichever
// goroutine produced it (a dispatch bridge, an elicitation request, etc).
// The SSE writer goroutine assigns the event its monotonically increasing
// id: field and appends it to the replay buffer as it is written out.
type SSEEvent struct {
	// Event is the SSE "event:" field. Empty means the default "message"
	// event, used for JSON-RPC responses/notifications.
	Event string
	// Data is the raw (already-serialized) SSE "data:" payload.
	Data string
}

// ClientInfo is the negotiated MCP client identity from initialize.
type ClientInfo struct {
	Name    string
	Version string
}

// ShellSessionEntry tracks server-side metadata for an active interactive
// shell session (Section 7). The PTY itself lives on the agent; the server
// only needs enough to route dispatches and serve shell://sessions.
type ShellSessionEntry struct {
	ClientID  string
	PID       int
	Shell     string
	CreatedAt time.Time
}

// Session holds all per-MCP-session state.
type Session struct {
	ID                string
	CreatedAt         time.Time
	NegotiatedVersion string
	ClientInfo        ClientInfo

	ReplayBuffer *ReplayBuffer
	EventCh      chan SSEEvent

	// CancelFunc cancels Ctx, signaling every goroutine scoped to this
	// session (the SSE writer, any in-flight dispatch bridges) to stop.
	Ctx        context.Context
	CancelFunc context.CancelFunc

	mu              sync.Mutex
	lastActivityAt  time.Time
	shellSessionMap map[string]*ShellSessionEntry
	pending         map[string]chan json.RawMessage // requestId -> waiting handler (elicitation responses)
	cancelFuncs     map[string]cancelEntry          // client requestId -> cancel for an in-flight tools/call
	subscriptions   map[string]bool                 // resource URI -> subscribed (resources/subscribe)
}

// cancelEntry is a registered CancelFunc plus whether it belongs to a
// detached (pattern (a)) job that outlives its originating tools/call.
type cancelEntry struct {
	fn       context.CancelFunc
	detached bool
}

// New constructs a Session with the given ID and replay buffer capacity.
// The returned Session's Ctx is derived from parent and is cancelled by
// either CancelFunc or the parent being cancelled.
func New(parent context.Context, id string, replayBufferSize int) *Session {
	ctx, cancel := context.WithCancel(parent)
	now := time.Now().UTC()
	return &Session{
		ID:              id,
		CreatedAt:       now,
		lastActivityAt:  now,
		ReplayBuffer:    NewReplayBuffer(replayBufferSize),
		EventCh:         make(chan SSEEvent, EventChBufferSize),
		Ctx:             ctx,
		CancelFunc:      cancel,
		shellSessionMap: map[string]*ShellSessionEntry{},
		pending:         map[string]chan json.RawMessage{},
		cancelFuncs:     map[string]cancelEntry{},
		subscriptions:   map[string]bool{},
	}
}

// Subscribe marks this session as subscribed to a resource URI
// (resources/subscribe, Section 4).
func (s *Session) Subscribe(uri string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscriptions[uri] = true
}

// Unsubscribe removes a resource subscription.
func (s *Session) Unsubscribe(uri string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscriptions, uri)
}

// IsSubscribed reports whether this session subscribed to uri.
func (s *Session) IsSubscribed(uri string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscriptions[uri]
}

// Touch updates LastActivityAt to now.
func (s *Session) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActivityAt = time.Now().UTC()
}

// LastActivityAt returns the last time Touch was called.
func (s *Session) LastActivityAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActivityAt
}

// SetShellSession records/removes a shell session mapping.
func (s *Session) SetShellSession(id string, entry *ShellSessionEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shellSessionMap[id] = entry
}

// GetShellSession returns the shell session entry for id, if any.
func (s *Session) GetShellSession(id string) (*ShellSessionEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.shellSessionMap[id]
	return e, ok
}

// DeleteShellSession removes a shell session mapping.
func (s *Session) DeleteShellSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.shellSessionMap, id)
}

// ListShellSessions returns a snapshot of all active shell session entries,
// keyed by shellSessionId.
func (s *Session) ListShellSessions() map[string]*ShellSessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*ShellSessionEntry, len(s.shellSessionMap))
	for k, v := range s.shellSessionMap {
		out[k] = v
	}
	return out
}

// AwaitResponse registers a channel that a future client POST carrying a
// JSON-RPC response with the given requestId will deliver to. Used for
// routing elicitation responses (Section 11) and, generally, any
// server-initiated request answered by the client over POST /mcp.
//
// Callers must eventually call CancelAwait(requestId) to release the
// registration (e.g. on timeout) even if a response was already delivered.
func (s *Session) AwaitResponse(requestID string) <-chan json.RawMessage {
	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[requestID] = ch
	s.mu.Unlock()
	return ch
}

// CancelAwait removes a pending response registration.
func (s *Session) CancelAwait(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, requestID)
}

// DeliverResponse routes a client-sent JSON-RPC response to the handler
// waiting on requestId, if any. Returns false if nothing is waiting (e.g.
// it already timed out or the ID is unknown).
func (s *Session) DeliverResponse(requestID string, raw json.RawMessage) bool {
	s.mu.Lock()
	ch, ok := s.pending[requestID]
	if ok {
		delete(s.pending, requestID)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	ch <- raw
	return true
}

// RegisterCancel associates a client JSON-RPC requestId with the
// CancelFunc for its in-flight tools/call, so a subsequent
// notifications/cancelled for that requestId can cancel it.
func (s *Session) RegisterCancel(requestID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelFuncs[requestID] = cancelEntry{fn: cancel}
}

// DetachCancel registers cancel for requestID as a detached job (dispatch
// pattern (a), Section 9): the registration survives the transport's
// UnregisterCancel when the tools/call returns, so a later
// notifications/cancelled for the original requestId still reaches the
// background job. The job's own cleanup must call RemoveDetachedCancel.
func (s *Session) DetachCancel(requestID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelFuncs[requestID] = cancelEntry{fn: cancel, detached: true}
}

// UnregisterCancel removes a requestId's registered CancelFunc (call this
// once the request completes, regardless of outcome). Detached
// registrations (DetachCancel) are left in place -- their owning job
// removes them via RemoveDetachedCancel when it finishes.
func (s *Session) UnregisterCancel(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.cancelFuncs[requestID]; ok && !entry.detached {
		delete(s.cancelFuncs, requestID)
	}
}

// RemoveDetachedCancel removes a registration regardless of detachment.
// Called by a detached job's cleanup once it reaches a terminal state.
func (s *Session) RemoveDetachedCancel(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancelFuncs, requestID)
}

// Cancel cancels the in-flight request registered under requestId, if any.
// Returns false if no such request is currently registered.
func (s *Session) Cancel(requestID string) bool {
	s.mu.Lock()
	entry, ok := s.cancelFuncs[requestID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	entry.fn()
	return true
}

// Emit sends an SSEEvent to EventCh, applying the backpressure policy from
// Section 8: if the channel is full, block for up to backpressureWait, then
// drop the event and return false so the caller can log a warning.
func (s *Session) Emit(ev SSEEvent, backpressureWait time.Duration) bool {
	select {
	case s.EventCh <- ev:
		return true
	default:
	}

	timer := time.NewTimer(backpressureWait)
	defer timer.Stop()
	select {
	case s.EventCh <- ev:
		return true
	case <-timer.C:
		return false
	case <-s.Ctx.Done():
		return false
	}
}
