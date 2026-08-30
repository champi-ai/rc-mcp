package jobs

import (
	"crypto/sha256"
	"encoding/hex"
)

// IdempotencyKey computes the idempotency key for a dispatched job:
//
//	SHA-256(sessionId + ":" + tool + ":" + requestId)
//
// per docs/specs/backend.md Section 9 ("Idempotency"), where requestId is
// the JSON-RPC request id from the originating tools/call.
func IdempotencyKey(sessionID, tool, requestID string) string {
	h := sha256.Sum256([]byte(sessionID + ":" + tool + ":" + requestID))
	return hex.EncodeToString(h[:])
}
