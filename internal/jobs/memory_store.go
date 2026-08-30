package jobs

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// DefaultJobTimeout is used for a job whose TimeoutSecs is <= 0
// (JOB_TIMEOUT default: 300s per Section 9).
const DefaultJobTimeout = 300 * time.Second

// MemoryStore is an in-process JobStore backed by a mutex-protected map,
// suitable for the default single-instance deployment (Section 9/10).
//
// Every job gets a timeout timer: if no terminal UpdateStatus call arrives
// within the job's timeout window, the store marks it failed with reason
// "timeout" and invokes CancelFn (if set) to ask the agent to stop.
type MemoryStore struct {
	defaultTimeout time.Duration
	cancelFn       CancelFunc

	// OnUpdate, if set, is invoked (outside the store's lock) with a clone
	// of the job after every status change, including timeout-driven ones.
	// Used to push job://{id} resource update notifications (Section 4.2).
	// Set before first use; not synchronized with concurrent updates.
	OnUpdate func(job *Job)

	mu        sync.Mutex
	jobs      map[string]*Job
	byIdemKey map[string]string // idempotencyKey -> jobID
	bySession map[string][]string
	timers    map[string]*time.Timer
}

// NewMemoryStore constructs a MemoryStore. defaultTimeout <= 0 uses
// DefaultJobTimeout for jobs that don't specify their own TimeoutSecs.
// cancelFn may be nil (e.g. in tests that don't care about the cancel
// side-effect); it is invoked when a job times out.
func NewMemoryStore(defaultTimeout time.Duration, cancelFn CancelFunc) *MemoryStore {
	if defaultTimeout <= 0 {
		defaultTimeout = DefaultJobTimeout
	}
	return &MemoryStore{
		defaultTimeout: defaultTimeout,
		cancelFn:       cancelFn,
		jobs:           make(map[string]*Job),
		byIdemKey:      make(map[string]string),
		bySession:      make(map[string][]string),
		timers:         make(map[string]*time.Timer),
	}
}

func (s *MemoryStore) Create(job *Job) error {
	if job == nil || job.ID == "" {
		return ErrNotFound
	}

	s.mu.Lock()

	if job.IdempotencyKey != "" {
		if existingID, ok := s.byIdemKey[job.IdempotencyKey]; ok {
			if existing, ok := s.jobs[existingID]; ok && existing.Status != JobStatusFailed {
				s.mu.Unlock()
				return ErrDuplicateIdempotencyKey
			}
		}
	}

	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.Status == "" {
		job.Status = JobStatusPending
	}

	stored := job.Clone()
	s.jobs[stored.ID] = stored
	if stored.IdempotencyKey != "" {
		s.byIdemKey[stored.IdempotencyKey] = stored.ID
	}
	s.bySession[stored.SessionID] = append(s.bySession[stored.SessionID], stored.ID)

	timeout := s.defaultTimeout
	if stored.TimeoutSecs > 0 {
		timeout = time.Duration(stored.TimeoutSecs) * time.Second
	}
	s.timers[stored.ID] = time.AfterFunc(timeout, func() {
		s.onTimeout(stored.ID)
	})

	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) onTimeout(id string) {
	s.mu.Lock()
	job, ok := s.jobs[id]
	if !ok || job.Status.IsTerminal() {
		s.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	job.Status = JobStatusFailed
	job.Error = "timeout"
	job.UpdatedAt = now
	job.CompletedAt = &now
	clientID, correlationID := job.ClientID, job.CorrelationID
	delete(s.timers, id)
	cancelFn := s.cancelFn
	updated := job.Clone()
	s.mu.Unlock()

	if cancelFn != nil {
		cancelFn(clientID, correlationID, "timeout")
	}
	if s.OnUpdate != nil {
		s.OnUpdate(updated)
	}
}

func (s *MemoryStore) Get(id string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return job.Clone(), nil
}

func (s *MemoryStore) UpdateStatus(id string, status JobStatus, result json.RawMessage, errMsg string) error {
	s.mu.Lock()
	job, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}

	job.Status = status
	job.UpdatedAt = time.Now().UTC()
	if result != nil {
		job.Result = append(json.RawMessage(nil), result...)
	}
	if errMsg != "" {
		job.Error = errMsg
	}
	if status.IsTerminal() {
		now := job.UpdatedAt
		job.CompletedAt = &now
		if t, ok := s.timers[id]; ok {
			t.Stop()
			delete(s.timers, id)
		}
	}
	updated := job.Clone()
	s.mu.Unlock()

	if s.OnUpdate != nil {
		s.OnUpdate(updated)
	}
	return nil
}

func (s *MemoryStore) ListBySession(sessionID string, limit int) ([]*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := s.bySession[sessionID]
	out := make([]*Job, 0, len(ids))
	for _, id := range ids {
		if job, ok := s.jobs[id]; ok {
			out = append(out, job.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) FindByIdempotencyKey(key string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byIdemKey[key]
	if !ok {
		return nil, false
	}
	job, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	return job.Clone(), true
}
