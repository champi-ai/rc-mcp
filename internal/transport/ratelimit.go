package transport

import (
	"sync"
	"time"
)

// Rate limiting defaults, per docs/specs/backend.md Section 12.5.
const (
	DefaultRateLimitSession        = 120 // requests per MCP session per minute
	DefaultRateLimitTools          = 60  // tool calls per MCP session per minute
	DefaultMaxConcurrentDispatches = 10  // concurrent dispatches per MCP session
)

// window is the sliding window used for the per-minute limits.
const window = time.Minute

// RateLimiter enforces per-session request/minute, tool-call/minute, and
// concurrent-dispatch limits. A single RateLimiter is shared by the
// transport Handler across all sessions.
type RateLimiter struct {
	sessionLimit  int
	toolLimit     int
	maxConcurrent int

	mu       sync.Mutex
	sessions map[string]*sessionLimitState
}

type sessionLimitState struct {
	requestTimes []time.Time
	toolTimes    []time.Time
	concurrent   int
}

// NewRateLimiter constructs a RateLimiter. Non-positive limits fall back to
// their package defaults.
func NewRateLimiter(sessionLimit, toolLimit, maxConcurrent int) *RateLimiter {
	if sessionLimit <= 0 {
		sessionLimit = DefaultRateLimitSession
	}
	if toolLimit <= 0 {
		toolLimit = DefaultRateLimitTools
	}
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrentDispatches
	}
	return &RateLimiter{
		sessionLimit:  sessionLimit,
		toolLimit:     toolLimit,
		maxConcurrent: maxConcurrent,
		sessions:      map[string]*sessionLimitState{},
	}
}

func (rl *RateLimiter) stateLocked(sessionID string) *sessionLimitState {
	st, ok := rl.sessions[sessionID]
	if !ok {
		st = &sessionLimitState{}
		rl.sessions[sessionID] = st
	}
	return st
}

// AllowRequest reports whether sessionID may make one more request this
// minute (all POST/GET/DELETE requests count), recording it if so.
func (rl *RateLimiter) AllowRequest(sessionID string) bool {
	return rl.allow(sessionID, false)
}

// AllowToolCall reports whether sessionID may make one more tools/call
// this minute, recording it if so.
func (rl *RateLimiter) AllowToolCall(sessionID string) bool {
	return rl.allow(sessionID, true)
}

func (rl *RateLimiter) allow(sessionID string, isTool bool) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	st := rl.stateLocked(sessionID)
	now := time.Now()

	if isTool {
		st.toolTimes = prune(st.toolTimes, now)
		if len(st.toolTimes) >= rl.toolLimit {
			return false
		}
		st.toolTimes = append(st.toolTimes, now)
		return true
	}

	st.requestTimes = prune(st.requestTimes, now)
	if len(st.requestTimes) >= rl.sessionLimit {
		return false
	}
	st.requestTimes = append(st.requestTimes, now)
	return true
}

// AcquireDispatchSlot reserves one of sessionID's concurrent-dispatch
// slots, returning false (without blocking or queuing) if the session is
// already at its concurrency cap. Callers that acquire a slot must call
// ReleaseDispatchSlot exactly once when the dispatch completes.
func (rl *RateLimiter) AcquireDispatchSlot(sessionID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	st := rl.stateLocked(sessionID)
	if st.concurrent >= rl.maxConcurrent {
		return false
	}
	st.concurrent++
	return true
}

// ReleaseDispatchSlot releases a concurrent-dispatch slot previously
// acquired via AcquireDispatchSlot.
func (rl *RateLimiter) ReleaseDispatchSlot(sessionID string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	st, ok := rl.sessions[sessionID]
	if !ok || st.concurrent <= 0 {
		return
	}
	st.concurrent--
}

// prune drops entries older than window, preserving order.
func prune(times []time.Time, now time.Time) []time.Time {
	cut := 0
	for cut < len(times) && now.Sub(times[cut]) >= window {
		cut++
	}
	if cut == 0 {
		return times
	}
	return append([]time.Time(nil), times[cut:]...)
}
