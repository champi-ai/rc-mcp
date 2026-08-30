package jobs

import (
	"encoding/json"
	"errors"
)

// ErrNotFound is returned by Get when the job ID does not exist.
var ErrNotFound = errors.New("jobs: not found")

// ErrDuplicateIdempotencyKey is returned by Create when a non-terminal-failed
// job already exists for the same idempotency key. Callers should use
// FindByIdempotencyKey to fetch the existing job rather than treating this
// as a hard error (Section 9, "Idempotency").
var ErrDuplicateIdempotencyKey = errors.New("jobs: duplicate idempotency key")

// CancelFunc is invoked by a JobStore implementation to ask the owning
// agent connection to cancel an in-flight job (e.g. on JOB_TIMEOUT).
// clientID/correlationID identify the target device and the wire-protocol
// dispatch to cancel.
type CancelFunc func(clientID, correlationID, reason string)

// JobStore persists job state for retrieval after completion and enforces
// idempotency + timeout policy, per docs/specs/backend.md Section 6/9.
type JobStore interface {
	// Create inserts job (status must already be set, typically
	// JobStatusPending). If a non-failed job already exists for
	// job.IdempotencyKey, Create returns ErrDuplicateIdempotencyKey and does
	// not insert a new record -- callers should then call
	// FindByIdempotencyKey to retrieve the existing job.
	Create(job *Job) error

	// Get returns the job with the given ID, or ErrNotFound.
	Get(id string) (*Job, error)

	// UpdateStatus transitions the job to status, optionally attaching a
	// terminal result or error message, and stamps UpdatedAt (and
	// CompletedAt, if status is terminal). Returns ErrNotFound if the job
	// does not exist.
	UpdateStatus(id string, status JobStatus, result json.RawMessage, errMsg string) error

	// ListBySession returns up to limit jobs for sessionID, most recent
	// first. limit <= 0 means no limit.
	ListBySession(sessionID string, limit int) ([]*Job, error)

	// FindByIdempotencyKey returns the existing job for key, if any job
	// (in any status) currently holds it. ok is false if none exists.
	FindByIdempotencyKey(key string) (job *Job, ok bool)
}
