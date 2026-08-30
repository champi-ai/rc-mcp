package transport

import (
	"sync"
	"testing"
)

func TestRateLimiter_AllowRequest_EnforcesSessionLimit(t *testing.T) {
	rl := NewRateLimiter(3, 100, 10)
	for i := 0; i < 3; i++ {
		if !rl.AllowRequest("sess-1") {
			t.Fatalf("request %d: want allowed", i)
		}
	}
	if rl.AllowRequest("sess-1") {
		t.Fatal("4th request within the window: want denied")
	}
}

func TestRateLimiter_AllowToolCall_EnforcesToolLimit(t *testing.T) {
	rl := NewRateLimiter(1000, 2, 10)
	firstOK := rl.AllowToolCall("sess-1")
	secondOK := rl.AllowToolCall("sess-1")
	if !firstOK || !secondOK {
		t.Fatal("first two tool calls: want allowed")
	}
	if rl.AllowToolCall("sess-1") {
		t.Fatal("3rd tool call within the window: want denied")
	}
}

func TestRateLimiter_LimitsArePerSession(t *testing.T) {
	rl := NewRateLimiter(1, 1000, 10)
	if !rl.AllowRequest("sess-a") {
		t.Fatal("sess-a first request: want allowed")
	}
	if !rl.AllowRequest("sess-b") {
		t.Fatal("sess-b first request: want allowed (independent budget)")
	}
	if rl.AllowRequest("sess-a") {
		t.Fatal("sess-a second request: want denied")
	}
}

func TestRateLimiter_ConcurrentDispatchCap(t *testing.T) {
	rl := NewRateLimiter(1000, 1000, 2)
	firstOK := rl.AcquireDispatchSlot("sess-1")
	secondOK := rl.AcquireDispatchSlot("sess-1")
	if !firstOK || !secondOK {
		t.Fatal("first two acquires: want success")
	}
	if rl.AcquireDispatchSlot("sess-1") {
		t.Fatal("3rd concurrent dispatch: want rejected, not queued")
	}
	rl.ReleaseDispatchSlot("sess-1")
	if !rl.AcquireDispatchSlot("sess-1") {
		t.Fatal("after release: want a slot available again")
	}
}

func TestRateLimiter_Defaults(t *testing.T) {
	rl := NewRateLimiter(0, 0, 0)
	if rl.sessionLimit != DefaultRateLimitSession {
		t.Errorf("sessionLimit = %d, want %d", rl.sessionLimit, DefaultRateLimitSession)
	}
	if rl.toolLimit != DefaultRateLimitTools {
		t.Errorf("toolLimit = %d, want %d", rl.toolLimit, DefaultRateLimitTools)
	}
	if rl.maxConcurrent != DefaultMaxConcurrentDispatches {
		t.Errorf("maxConcurrent = %d, want %d", rl.maxConcurrent, DefaultMaxConcurrentDispatches)
	}
}

func TestRateLimiter_ConcurrentAccessRace(t *testing.T) {
	rl := NewRateLimiter(10000, 10000, 10000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rl.AllowRequest("sess-1")
			rl.AllowToolCall("sess-1")
			if rl.AcquireDispatchSlot("sess-1") {
				rl.ReleaseDispatchSlot("sess-1")
			}
		}()
	}
	wg.Wait()
}
