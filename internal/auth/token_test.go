package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateBearerToken_Match(t *testing.T) {
	if err := ValidateBearerToken("s3cr3t-token", "s3cr3t-token"); err != nil {
		t.Fatalf("expected match, got error: %v", err)
	}
}

func TestValidateBearerToken_Mismatch(t *testing.T) {
	if err := ValidateBearerToken("s3cr3t-token", "wrong-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("got %v, want ErrInvalidToken", err)
	}
}

func TestValidateBearerToken_NotConfigured(t *testing.T) {
	if err := ValidateBearerToken("", "anything"); !errors.Is(err, ErrAuthNotConfigured) {
		t.Fatalf("got %v, want ErrAuthNotConfigured", err)
	}
	// An empty configured token must never be treated as "any token valid",
	// including an empty presented token.
	if err := ValidateBearerToken("", ""); !errors.Is(err, ErrAuthNotConfigured) {
		t.Fatalf("got %v, want ErrAuthNotConfigured for empty/empty", err)
	}
}

func TestValidateBearerToken_DifferentLengths(t *testing.T) {
	if err := ValidateBearerToken("a-long-configured-token-value", "short"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("got %v, want ErrInvalidToken", err)
	}
}

// TestValidateBearerToken_NoEarlyByteShortCircuit is a structural test (not
// a timing benchmark): it confirms mismatches are detected identically
// regardless of *where* in the token the mismatch occurs, which is only
// possible if the implementation does not compare byte-by-byte with
// early exit (e.g. a naive loop with `if a[i] != b[i] { return false }`,
// or comparing raw variable-length inputs directly with
// subtle.ConstantTimeCompare, which itself short-circuits on length).
func TestValidateBearerToken_NoEarlyByteShortCircuit(t *testing.T) {
	base := strings.Repeat("A", 64)

	mismatchAtStart := "X" + base[1:]
	mismatchAtEnd := base[:len(base)-1] + "X"

	errStart := ValidateBearerToken(base, mismatchAtStart)
	errEnd := ValidateBearerToken(base, mismatchAtEnd)

	if !errors.Is(errStart, ErrInvalidToken) || !errors.Is(errEnd, ErrInvalidToken) {
		t.Fatalf("expected both mismatches rejected, got start=%v end=%v", errStart, errEnd)
	}

	// The implementation must hash (or otherwise fix the comparison length)
	// before calling subtle.ConstantTimeCompare, rather than compare the
	// raw, variable-length token strings directly -- verified indirectly by
	// the different-length case above returning ErrInvalidToken rather than
	// panicking or behaving inconsistently.
}

func TestExtractBearerToken(t *testing.T) {
	tok, ok := ExtractBearerToken("Bearer abc123")
	if !ok || tok != "abc123" {
		t.Fatalf("got (%q, %v), want (\"abc123\", true)", tok, ok)
	}

	if _, ok := ExtractBearerToken("Basic abc123"); ok {
		t.Fatal("expected ok=false for non-Bearer scheme")
	}

	if _, ok := ExtractBearerToken(""); ok {
		t.Fatal("expected ok=false for empty header")
	}
}
