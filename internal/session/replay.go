package session

import "sync"

// StoredEvent is a single SSE event retained in a ReplayBuffer for
// Last-Event-ID replay on reconnect. See docs/specs/backend.md Section 2
// ("Resumability").
type StoredEvent struct {
	ID    int64
	Event string
	Data  string
}

// ReplayBuffer is a circular buffer of the most recent SSE events emitted
// on a session's stream, used to serve Last-Event-ID replay when an MCP
// client reconnects. IDs are monotonically increasing, starting at 1.
//
// ReplayBuffer is safe for concurrent use, but in practice only the active
// SSE writer goroutine for a session ever calls Append (Section 8:
// "1 [SSE writer] per MCP session").
type ReplayBuffer struct {
	mu       sync.Mutex
	capacity int
	events   []StoredEvent
	nextID   int64
}

// NewReplayBuffer constructs a ReplayBuffer retaining at most capacity
// events. capacity <= 0 is treated as 1 (a functioning, if useless, buffer
// is preferable to a panic).
func NewReplayBuffer(capacity int) *ReplayBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &ReplayBuffer{capacity: capacity, nextID: 1}
}

// Append assigns the next monotonically increasing ID to a new event,
// stores it, and returns the stored copy (with its assigned ID).
func (b *ReplayBuffer) Append(event, data string) StoredEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	ev := StoredEvent{ID: b.nextID, Event: event, Data: data}
	b.nextID++

	b.events = append(b.events, ev)
	if len(b.events) > b.capacity {
		// Drop the oldest entries beyond capacity. Reslicing (rather than a
		// true ring index) is simplicity over micro-optimization; capacity
		// is small (default 500) and Append is not a hot path.
		drop := len(b.events) - b.capacity
		b.events = append([]StoredEvent(nil), b.events[drop:]...)
	}
	return ev
}

// Since returns all retained events with ID > lastEventID, in order.
//
// ok is false if replay is not possible: lastEventID refers to a point
// before the buffer's retained window, meaning at least one event between
// lastEventID and the oldest retained event was already evicted. Per
// Section 2, callers should respond 204 No Content in that case and the
// client should re-initialize.
func (b *ReplayBuffer) Since(lastEventID int64) ([]StoredEvent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.events) == 0 {
		// Nothing has ever been emitted. Replay is only trivially possible
		// if the client isn't missing anything (lastEventID is already
		// caught up with "nothing sent yet").
		if lastEventID >= b.nextID-1 {
			return nil, true
		}
		return nil, false
	}

	oldest := b.events[0].ID
	if lastEventID < oldest-1 {
		// A gap: at least event (oldest-1) existed and was evicted before
		// the client saw it.
		return nil, false
	}

	out := make([]StoredEvent, 0, len(b.events))
	for _, e := range b.events {
		if e.ID > lastEventID {
			out = append(out, e)
		}
	}
	return out, true
}

// NextID returns the ID that will be assigned to the next appended event,
// primarily for tests/introspection.
func (b *ReplayBuffer) NextID() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextID
}
