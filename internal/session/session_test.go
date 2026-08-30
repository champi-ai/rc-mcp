package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSession_ShellSessionMap(t *testing.T) {
	s := New(context.Background(), "sess-1", 10)
	s.SetShellSession("shell-1", &ShellSessionEntry{ClientID: "dev-1", PID: 123, Shell: "/bin/bash", CreatedAt: time.Now()})

	entry, ok := s.GetShellSession("shell-1")
	if !ok || entry.ClientID != "dev-1" {
		t.Fatalf("GetShellSession = %+v, %v", entry, ok)
	}

	if len(s.ListShellSessions()) != 1 {
		t.Fatalf("ListShellSessions len = %d, want 1", len(s.ListShellSessions()))
	}

	s.DeleteShellSession("shell-1")
	if _, ok := s.GetShellSession("shell-1"); ok {
		t.Fatal("shell session should be removed")
	}
}

func TestSession_AwaitAndDeliverResponse(t *testing.T) {
	s := New(context.Background(), "sess-1", 10)
	ch := s.AwaitResponse("req-1")

	if delivered := s.DeliverResponse("req-1", json.RawMessage(`{"result":true}`)); !delivered {
		t.Fatal("DeliverResponse: want true")
	}

	select {
	case raw := <-ch:
		if string(raw) != `{"result":true}` {
			t.Fatalf("got %s", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivered response")
	}

	// Delivering again (nothing waiting) returns false.
	if delivered := s.DeliverResponse("req-1", json.RawMessage(`{}`)); delivered {
		t.Fatal("DeliverResponse for unknown/already-consumed id: want false")
	}
}

func TestSession_CancelAwaitStopsDelivery(t *testing.T) {
	s := New(context.Background(), "sess-1", 10)
	s.AwaitResponse("req-1")
	s.CancelAwait("req-1")

	if delivered := s.DeliverResponse("req-1", json.RawMessage(`{}`)); delivered {
		t.Fatal("DeliverResponse after CancelAwait: want false")
	}
}

func TestSession_RegisterAndCancel(t *testing.T) {
	s := New(context.Background(), "sess-1", 10)
	ctx, cancel := context.WithCancel(context.Background())
	s.RegisterCancel("req-1", cancel)

	if !s.Cancel("req-1") {
		t.Fatal("Cancel: want true")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("context should be cancelled")
	}

	// The registration persists until UnregisterCancel is explicitly
	// called (by the tools/call handler once the request completes), so a
	// second Cancel of the same requestId still finds it.
	if !s.Cancel("req-1") {
		t.Fatal("Cancel before UnregisterCancel: want true")
	}
	s.UnregisterCancel("req-1")
	if s.Cancel("req-1") {
		t.Fatal("Cancel after UnregisterCancel: want false")
	}
}

func TestSession_EmitAndBackpressure(t *testing.T) {
	s := New(context.Background(), "sess-1", 10)

	// Fill the channel.
	for i := 0; i < EventChBufferSize; i++ {
		if !s.Emit(SSEEvent{Data: "x"}, time.Second) {
			t.Fatalf("Emit %d: want true (channel not yet full)", i)
		}
	}

	// One more should block then drop after the wait elapses.
	start := time.Now()
	ok := s.Emit(SSEEvent{Data: "overflow"}, 100*time.Millisecond)
	elapsed := time.Since(start)
	if ok {
		t.Fatal("Emit on full channel: want false (dropped)")
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("Emit returned after %v, want >= 100ms (should have waited)", elapsed)
	}
}

func TestSession_TouchUpdatesLastActivity(t *testing.T) {
	s := New(context.Background(), "sess-1", 10)
	first := s.LastActivityAt()
	time.Sleep(5 * time.Millisecond)
	s.Touch()
	if !s.LastActivityAt().After(first) {
		t.Fatal("Touch should advance LastActivityAt")
	}
}
