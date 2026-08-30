package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
)

// DefaultReplayBufferSize is used when a MemoryStore is constructed with a
// non-positive replay buffer size (SSE_REPLAY_BUFFER_SIZE default: 500).
const DefaultReplayBufferSize = 500

// MemoryStore is an in-process SessionStore backed by a sync.Map, per
// docs/specs/backend.md Section 7 ("Backing implementations").
type MemoryStore struct {
	replayBufferSize int

	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewMemoryStore constructs a MemoryStore. replayBufferSize <= 0 uses
// DefaultReplayBufferSize.
func NewMemoryStore(replayBufferSize int) *MemoryStore {
	if replayBufferSize <= 0 {
		replayBufferSize = DefaultReplayBufferSize
	}
	return &MemoryStore{
		replayBufferSize: replayBufferSize,
		sessions:         map[string]*Session{},
	}
}

// Create implements SessionStore.
func (m *MemoryStore) Create(ctx context.Context) (*Session, error) {
	id, err := generateSessionID()
	if err != nil {
		return nil, err
	}
	s := New(ctx, id, m.replayBufferSize)

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	return s, nil
}

// Get implements SessionStore.
func (m *MemoryStore) Get(ctx context.Context, id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

// Touch implements SessionStore.
func (m *MemoryStore) Touch(ctx context.Context, id string) error {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	s.Touch()
	return nil
}

// Delete implements SessionStore.
func (m *MemoryStore) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if ok {
		s.CancelFunc()
	}
	return nil
}

// Range implements SessionStore.
func (m *MemoryStore) Range(fn func(*Session) bool) {
	m.mu.RLock()
	snapshot := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		snapshot = append(snapshot, s)
	}
	m.mu.RUnlock()

	for _, s := range snapshot {
		if !fn(s) {
			return
		}
	}
}

var _ SessionStore = (*MemoryStore)(nil)

// generateSessionID returns a 128-bit cryptographically random,
// hex-encoded (32 character) Mcp-Session-Id per docs/specs/backend.md
// Section 2.
func generateSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
