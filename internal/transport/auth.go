// Package transport implements the MCP-facing Streamable HTTP transport:
// the single /mcp endpoint (POST/GET/DELETE), its SSE stream, and the
// bearer-auth / origin-allowlist middleware that gate every request. See
// docs/specs/backend.md Section 2 and Section 12.
package transport

import (
	"net/http"

	"github.com/champi-ai/rc-mcp/internal/auth"
)

// AuthMiddleware validates the Authorization: Bearer <token> header on
// every request against token using constant-time comparison. On failure
// it writes a JSON-RPC error response with code -32002 and does not call
// next. See docs/specs/backend.md Section 12.1.
//
// token must be non-empty; callers (cmd/server) must refuse to start the
// server if AUTH_TOKEN is unset, per the spec -- this middleware does not
// itself special-case an empty token as "open access".
func AuthMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := auth.ExtractBearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeAuthError(w)
			return
		}
		if err := auth.ValidateBearerToken(token, presented); err != nil {
			writeAuthError(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeAuthError(w http.ResponseWriter) {
	writeJSONRPCError(w, http.StatusUnauthorized, nil, codeAuthFailure, "Unauthorized")
}
