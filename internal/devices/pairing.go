package devices

import (
	"crypto/rand"
	"fmt"
	"time"
)

// pairingCodeAlphabet excludes visually ambiguous characters: 0/O, 1/I/L.
const pairingCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// generatePairingCode returns a human-readable pairing code in the format
// "XXXX-XXXX", uppercase alphanumeric, generated via crypto/rand, excluding
// ambiguous characters.
func generatePairingCode() (string, error) {
	buf := make([]byte, 8)
	idx := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pairing code: %w", err)
	}
	for i, b := range buf {
		idx[i] = pairingCodeAlphabet[int(b)%len(pairingCodeAlphabet)]
	}
	return fmt.Sprintf("%s-%s", idx[0:4], idx[4:8]), nil
}

// IsExpired reports whether the pairing code is past its TTL as of now.
// An unapproved code past its TTL is handled by callers (the agent hub) by
// sending an "error" envelope with code "pairing_expired" and closing the
// WebSocket, per docs/specs/backend.md Section 12.2.
func (pc *PairingCode) IsExpired(now time.Time) bool {
	return now.After(pc.ExpiresAt)
}
