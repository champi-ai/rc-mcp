package session

import "testing"

func TestReplayBuffer_AppendAssignsIncreasingIDs(t *testing.T) {
	b := NewReplayBuffer(10)
	e1 := b.Append("message", "a")
	e2 := b.Append("message", "b")
	if e1.ID != 1 || e2.ID != 2 {
		t.Fatalf("got IDs %d, %d; want 1, 2", e1.ID, e2.ID)
	}
}

func TestReplayBuffer_SinceReplaysMissedEvents(t *testing.T) {
	b := NewReplayBuffer(10)
	b.Append("message", "a") // id 1
	b.Append("message", "b") // id 2
	b.Append("message", "c") // id 3

	events, ok := b.Since(1)
	if !ok {
		t.Fatal("Since(1): want ok=true")
	}
	if len(events) != 2 || events[0].Data != "b" || events[1].Data != "c" {
		t.Fatalf("Since(1) = %+v, want [b, c]", events)
	}
}

func TestReplayBuffer_SinceCaughtUpReturnsEmpty(t *testing.T) {
	b := NewReplayBuffer(10)
	b.Append("message", "a")
	events, ok := b.Since(1)
	if !ok {
		t.Fatal("want ok=true")
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0", len(events))
	}
}

func TestReplayBuffer_SinceTooOldReturnsNotOK(t *testing.T) {
	b := NewReplayBuffer(2)
	for i := 0; i < 5; i++ {
		b.Append("message", "x")
	}
	// Buffer capacity 2 retains IDs 4,5. Asking for anything since 1 has a
	// gap (events 2,3 were evicted).
	_, ok := b.Since(1)
	if ok {
		t.Fatal("want ok=false for an ID older than the retained window")
	}

	// Since(3) still has a gap: event 3 was dropped before this client saw
	// it (only 4,5 remain, oldest=4, so oldest-1=3 is the boundary; asking
	// for anything less than that is a gap).
	_, ok = b.Since(2)
	if ok {
		t.Fatal("want ok=false for Since(2) when oldest retained is 4")
	}

	events, ok := b.Since(3)
	if !ok {
		t.Fatal("want ok=true for Since(3) (boundary: oldest-1)")
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
}

func TestReplayBuffer_EmptyBufferCaughtUp(t *testing.T) {
	b := NewReplayBuffer(10)
	events, ok := b.Since(0)
	if !ok || len(events) != 0 {
		t.Fatalf("Since(0) on empty buffer = %+v, %v; want [], true", events, ok)
	}
}
