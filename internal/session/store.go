package session

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get/Touch/Delete when the session ID does not
// exist (never created, already deleted, or expired).
var ErrNotFound = errors.New("session: not found")

// SessionStore manages the lifecycle of MCP sessions. See
// docs/specs/backend.md Section 7.
type SessionStore interface {
	// Create allocates a new Session with a fresh, cryptographically random
	// ID and stores it.
	Create(ctx context.Context) (*Session, error)
	// Get returns the session with the given ID, or ErrNotFound if it does
	// not exist (never created, or already expired/deleted).
	Get(ctx context.Context, id string) (*Session, error)
	// Touch records activity on the session, extending its idle-expiry
	// deadline. Returns ErrNotFound if the session does not exist.
	Touch(ctx context.Context, id string) error
	// Delete removes a session, cancelling its Ctx first so any goroutines
	// scoped to it (SSE writer, dispatch bridges) observe the cancellation.
	// Deleting an already-absent session is not an error.
	Delete(ctx context.Context, id string) error
	// Range calls fn for every currently stored session. fn returning false
	// stops iteration early. Used by the idle-expiry sweep.
	Range(fn func(*Session) bool)
}
