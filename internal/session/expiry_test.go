package session

import (
	"context"
	"testing"
	"time"
)

func TestSweepIdleSessions_ExpiresOldSessions(t *testing.T) {
	store := NewMemoryStore(10)
	ctx := context.Background()

	old, err := store.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fresh, err := store.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Force "old" to look idle by backdating its activity indirectly: we
	// can't set time directly (unexported), so use a near-zero timeout and
	// let real elapsed time exceed it, while "fresh" is Touch()ed right
	// before the sweep to stay under it.
	time.Sleep(20 * time.Millisecond)
	fresh.Touch()

	sweepIdleSessions(ctx, store, 10*time.Millisecond)

	if _, err := store.Get(ctx, old.ID); err != ErrNotFound {
		t.Fatalf("expected old session to be expired, got err=%v", err)
	}
	if _, err := store.Get(ctx, fresh.ID); err != nil {
		t.Fatalf("expected fresh session to survive, got err=%v", err)
	}

	select {
	case <-old.Ctx.Done():
	default:
		t.Fatal("expired session's Ctx should be cancelled")
	}
}

func TestRunIdleExpiry_StopsOnContextCancel(t *testing.T) {
	store := NewMemoryStore(10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		RunIdleExpiry(ctx, store, time.Minute)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunIdleExpiry did not return after context cancellation")
	}
}
