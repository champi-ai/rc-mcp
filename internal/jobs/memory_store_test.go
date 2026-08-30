package jobs

import (
	"testing"
	"time"
)

func newTestJob(id, sessionID, idemKey string) *Job {
	return &Job{
		ID:             id,
		SessionID:      sessionID,
		ClientID:       "device-1",
		Tool:           "screenshot_watch",
		Status:         JobStatusPending,
		IdempotencyKey: idemKey,
		CorrelationID:  id,
	}
}

func TestMemoryStore_CreateGet(t *testing.T) {
	s := NewMemoryStore(time.Minute, nil)

	job := newTestJob("job-1", "sess-1", "key-1")
	if err := s.Create(job); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get("job-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != JobStatusPending {
		t.Fatalf("Status = %v, want pending", got.Status)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("expected CreatedAt/UpdatedAt to be stamped")
	}
}

func TestMemoryStore_GetNotFound(t *testing.T) {
	s := NewMemoryStore(time.Minute, nil)
	if _, err := s.Get("nope"); err != ErrNotFound {
		t.Fatalf("Get(nope) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_LifecycleTransitions(t *testing.T) {
	s := NewMemoryStore(time.Minute, nil)
	job := newTestJob("job-2", "sess-1", "key-2")
	if err := s.Create(job); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.UpdateStatus("job-2", JobStatusRunning, nil, ""); err != nil {
		t.Fatalf("UpdateStatus(running): %v", err)
	}
	got, _ := s.Get("job-2")
	if got.Status != JobStatusRunning {
		t.Fatalf("Status = %v, want running", got.Status)
	}
	if got.CompletedAt != nil {
		t.Fatal("CompletedAt should be nil while running")
	}

	if err := s.UpdateStatus("job-2", JobStatusSucceeded, []byte(`{"ok":true}`), ""); err != nil {
		t.Fatalf("UpdateStatus(succeeded): %v", err)
	}
	got, _ = s.Get("job-2")
	if got.Status != JobStatusSucceeded {
		t.Fatalf("Status = %v, want succeeded", got.Status)
	}
	if got.CompletedAt == nil {
		t.Fatal("expected CompletedAt to be set on terminal status")
	}
	if string(got.Result) != `{"ok":true}` {
		t.Fatalf("Result = %s", got.Result)
	}
}

func TestMemoryStore_DuplicateIdempotencyKeyReturnsExisting(t *testing.T) {
	s := NewMemoryStore(time.Minute, nil)
	key := IdempotencyKey("sess-1", "screenshot_watch", "req-1")

	first := newTestJob("job-a", "sess-1", key)
	if err := s.Create(first); err != nil {
		t.Fatalf("Create(first): %v", err)
	}

	dup := newTestJob("job-b", "sess-1", key)
	err := s.Create(dup)
	if err != ErrDuplicateIdempotencyKey {
		t.Fatalf("Create(dup) = %v, want ErrDuplicateIdempotencyKey", err)
	}

	existing, ok := s.FindByIdempotencyKey(key)
	if !ok {
		t.Fatal("FindByIdempotencyKey: expected existing job")
	}
	if existing.ID != "job-a" {
		t.Fatalf("existing.ID = %s, want job-a", existing.ID)
	}

	// job-b should not have been inserted.
	if _, err := s.Get("job-b"); err != ErrNotFound {
		t.Fatalf("Get(job-b) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_DuplicateIdempotencyKeyAllowedAfterFailure(t *testing.T) {
	s := NewMemoryStore(time.Minute, nil)
	key := IdempotencyKey("sess-1", "screenshot_watch", "req-2")

	first := newTestJob("job-c", "sess-1", key)
	if err := s.Create(first); err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	if err := s.UpdateStatus("job-c", JobStatusFailed, nil, "boom"); err != nil {
		t.Fatalf("UpdateStatus(failed): %v", err)
	}

	retry := newTestJob("job-d", "sess-1", key)
	if err := s.Create(retry); err != nil {
		t.Fatalf("Create(retry after failure) should succeed, got: %v", err)
	}
}

func TestMemoryStore_ListBySession(t *testing.T) {
	s := NewMemoryStore(time.Minute, nil)
	_ = s.Create(newTestJob("job-1", "sess-x", "k1"))
	time.Sleep(2 * time.Millisecond)
	_ = s.Create(newTestJob("job-2", "sess-x", "k2"))
	_ = s.Create(newTestJob("job-3", "sess-y", "k3"))

	jobs, err := s.ListBySession("sess-x", 0)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(jobs))
	}
	// Most recent first.
	if jobs[0].ID != "job-2" {
		t.Fatalf("jobs[0].ID = %s, want job-2 (most recent first)", jobs[0].ID)
	}

	limited, err := s.ListBySession("sess-x", 1)
	if err != nil {
		t.Fatalf("ListBySession(limit=1): %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("len(limited) = %d, want 1", len(limited))
	}
}

func TestMemoryStore_TimeoutMarksFailedAndSendsCancel(t *testing.T) {
	var gotClientID, gotCorrelationID, gotReason string
	cancelled := make(chan struct{})
	cancelFn := func(clientID, correlationID, reason string) {
		gotClientID, gotCorrelationID, gotReason = clientID, correlationID, reason
		close(cancelled)
	}

	s := NewMemoryStore(20*time.Millisecond, cancelFn)
	job := newTestJob("job-timeout", "sess-1", "key-timeout")
	if err := s.Create(job); err != nil {
		t.Fatalf("Create: %v", err)
	}

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancelFn to be invoked")
	}

	got, err := s.Get("job-timeout")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != JobStatusFailed {
		t.Fatalf("Status = %v, want failed", got.Status)
	}
	if got.Error != "timeout" {
		t.Fatalf("Error = %q, want %q", got.Error, "timeout")
	}
	if got.CompletedAt == nil {
		t.Fatal("expected CompletedAt to be set")
	}
	if gotClientID != "device-1" || gotCorrelationID != "job-timeout" || gotReason != "timeout" {
		t.Fatalf("cancelFn args = (%s, %s, %s)", gotClientID, gotCorrelationID, gotReason)
	}
}

func TestMemoryStore_TimeoutDoesNotFireAfterTerminalUpdate(t *testing.T) {
	cancelCalled := false
	cancelFn := func(string, string, string) { cancelCalled = true }

	s := NewMemoryStore(30*time.Millisecond, cancelFn)
	job := newTestJob("job-early", "sess-1", "key-early")
	if err := s.Create(job); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.UpdateStatus("job-early", JobStatusSucceeded, []byte(`{}`), ""); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	time.Sleep(60 * time.Millisecond)

	if cancelCalled {
		t.Fatal("cancelFn should not be invoked after job already reached a terminal state")
	}
	got, _ := s.Get("job-early")
	if got.Status != JobStatusSucceeded {
		t.Fatalf("Status = %v, want succeeded (should not be overwritten by timeout)", got.Status)
	}
}

func TestIdempotencyKey_Deterministic(t *testing.T) {
	k1 := IdempotencyKey("sess-1", "screenshot_watch", "req-1")
	k2 := IdempotencyKey("sess-1", "screenshot_watch", "req-1")
	if k1 != k2 {
		t.Fatal("IdempotencyKey should be deterministic for the same inputs")
	}
	k3 := IdempotencyKey("sess-1", "screenshot_watch", "req-2")
	if k1 == k3 {
		t.Fatal("IdempotencyKey should differ for different requestIds")
	}
}
