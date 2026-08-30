// Package auth provides constant-time bearer token comparison primitives
// shared by every auth boundary in rc-mcp-server. See docs/specs/backend.md
// Section 12.1.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"
)

// ErrAuthNotConfigured is returned when the configured token is empty.
// Callers must treat this as "auth is not configured" -- refusing to start
// or refusing all requests -- never as "any token is valid".
var ErrAuthNotConfigured = errors.New("auth: no token configured")

// ErrInvalidToken is returned when the presented token does not match the
// configured token.
var ErrInvalidToken = errors.New("auth: invalid token")

// ValidateBearerToken validates presented against configured in constant
// time using crypto/subtle.ConstantTimeCompare.
//
// If configured is empty, ValidateBearerToken returns ErrAuthNotConfigured
// rather than treating an unset token as "anonymous access allowed". The
// caller (the MCP transport layer, in a later phase) is expected to refuse
// to start the server when this occurs.
func ValidateBearerToken(configured, presented string) error {
	if configured == "" {
		return ErrAuthNotConfigured
	}

	// Compare fixed-length SHA-256 digests rather than the raw tokens so
	// ConstantTimeCompare never short-circuits on a length mismatch, which
	// would otherwise leak the configured token's length.
	want := sha256.Sum256([]byte(configured))
	got := sha256.Sum256([]byte(presented))

	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		return ErrInvalidToken
	}
	return nil
}

// ExtractBearerToken parses the value of an Authorization header and
// returns the token from an "Bearer <token>" scheme. ok is false if the
// header does not use the Bearer scheme.
func ExtractBearerToken(authorizationHeader string) (token string, ok bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authorizationHeader, prefix) {
		return "", false
	}
	return strings.TrimPrefix(authorizationHeader, prefix), true
}
