package session

import (
	"context"
	"log"
	"time"
)

// DefaultIdleTimeout is used when RunIdleExpiry is called with a
// non-positive idleTimeout (MCP_SESSION_IDLE_TIMEOUT default: 30m).
const DefaultIdleTimeout = 30 * time.Minute

// idleSweepInterval is how often the expiry sweep runs (Section 7:
// "A background goroutine runs every 60 seconds").
const idleSweepInterval = 60 * time.Second

// RunIdleExpiry runs the idle-session expiry sweep every idleSweepInterval
// until ctx is cancelled. Sessions whose LastActivityAt is older than
// idleTimeout are expired: their Ctx is cancelled (which signals the SSE
// writer and any in-flight dispatch bridges to stop) and they are removed
// from store.
//
// Call this once, in its own goroutine, from cmd/server's startup.
func RunIdleExpiry(ctx context.Context, store SessionStore, idleTimeout time.Duration) {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}

	ticker := time.NewTicker(idleSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepIdleSessions(ctx, store, idleTimeout)
		}
	}
}

func sweepIdleSessions(ctx context.Context, store SessionStore, idleTimeout time.Duration) {
	now := time.Now().UTC()
	var expired []string
	store.Range(func(s *Session) bool {
		if now.Sub(s.LastActivityAt()) > idleTimeout {
			expired = append(expired, s.ID)
		}
		return true
	})

	for _, id := range expired {
		log.Printf("session: expiring idle session %s (idle > %s)", id, idleTimeout)
		if err := store.Delete(ctx, id); err != nil {
			log.Printf("session: failed to delete expired session %s: %v", id, err)
		}
	}
}
