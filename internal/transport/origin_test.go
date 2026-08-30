package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOriginMiddleware_NoAllowlistPermitsAny(t *testing.T) {
	h := OriginMiddleware(nil, okHandler())
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no allowlist configured)", rec.Code)
	}
}

func TestOriginMiddleware_NoOriginHeaderPermitted(t *testing.T) {
	h := OriginMiddleware([]string{"https://allowed.example.com"}, okHandler())
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no Origin header)", rec.Code)
	}
}

func TestOriginMiddleware_AllowedOrigin(t *testing.T) {
	h := OriginMiddleware([]string{"https://allowed.example.com"}, okHandler())
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Origin", "https://allowed.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestOriginMiddleware_RejectsDisallowedOrigin(t *testing.T) {
	h := OriginMiddleware([]string{"https://allowed.example.com"}, okHandler())
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}
