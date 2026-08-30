// Package jobs implements the server-side job store for long-running
// dispatch pattern (a) operations (e.g. screenshot_watch), per
// docs/specs/backend.md Section 9 ("Long-Running Tasks & Progress Push").
package jobs

import (
	"encoding/json"
	"time"
)

// JobStatus represents the lifecycle state of a long-running job.
type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusSucceeded JobStatus = "succeeded"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCancelled JobStatus = "cancelled"
)

// IsTerminal reports whether s is one of the terminal states
// (succeeded, failed, cancelled).
func (s JobStatus) IsTerminal() bool {
	switch s {
	case JobStatusSucceeded, JobStatusFailed, JobStatusCancelled:
		return true
	default:
		return false
	}
}

// Job is the envelope for long-running operations bridged through the
// server (Section 6, package internal/jobs).
type Job struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"sessionId"`
	ClientID       string          `json:"clientId"`
	Tool           string          `json:"tool"`
	Status         JobStatus       `json:"status"`
	Payload        json.RawMessage `json:"payload"`
	Result         json.RawMessage `json:"result,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey"`
	ProgressToken  string          `json:"progressToken,omitempty"`
	CorrelationID  string          `json:"correlationId"`
	TimeoutSecs    int             `json:"timeoutSecs"`
	Error          string          `json:"error,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	CompletedAt    *time.Time      `json:"completedAt,omitempty"`
}

// Clone returns a deep-enough copy of j suitable for returning from a
// JobStore without letting callers mutate internal state.
func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	c := *j
	if j.Payload != nil {
		c.Payload = append(json.RawMessage(nil), j.Payload...)
	}
	if j.Result != nil {
		c.Result = append(json.RawMessage(nil), j.Result...)
	}
	if j.CompletedAt != nil {
		t := *j.CompletedAt
		c.CompletedAt = &t
	}
	return &c
}

// ProgressEvent is received from the agent and bridged to the MCP client.
type ProgressEvent struct {
	JobID         string          `json:"jobId"`
	SessionID     string          `json:"sessionId"`
	ProgressToken string          `json:"progressToken"`
	Percent       *float64        `json:"percent,omitempty"`
	Message       string          `json:"message"`
	Data          json.RawMessage `json:"data,omitempty"`
	Terminal      bool            `json:"terminal"`
}

// IdempotencyKey computes SHA-256(sessionId + ":" + tool + ":" + requestId)
// per Section 9 ("Idempotency"). See idempotency.go for the implementation
// to keep this file focused on types.
