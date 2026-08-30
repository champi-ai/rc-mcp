package transport

import "net/http"

// OriginMiddleware validates the Origin header against an allowlist to
// prevent DNS rebinding attacks (docs/specs/backend.md Section 12.3).
//
// If allowed is empty, origin validation is not enforced (the deployment
// has not configured MCP_ALLOWED_ORIGINS; most non-browser MCP clients
// don't send an Origin header at all). If the request carries no Origin
// header, it is allowed through regardless of allowed (non-browser
// clients). If an Origin header is present, it must exactly match one of
// the allowed entries.
func OriginMiddleware(allowed []string, next http.Handler) http.Handler {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		allowedSet[o] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || len(allowedSet) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := allowedSet[origin]; !ok {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
