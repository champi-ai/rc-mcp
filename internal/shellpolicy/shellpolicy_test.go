package shellpolicy

import (
	"strings"
	"testing"
)

func TestNew_NilPolicyAllowsEverything(t *testing.T) {
	p, err := New(nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if allowed, reason := p.Check("rm -rf /"); !allowed || reason != "" {
		t.Fatalf("allowed=%v reason=%q, want allowed with no configured policy", allowed, reason)
	}
}

func TestCheck_NilReceiverAllowsEverything(t *testing.T) {
	var p *Policy
	if allowed, _ := p.Check("anything"); !allowed {
		t.Fatal("a nil *Policy must allow everything")
	}
}

func TestDenylist_BlocksMatch(t *testing.T) {
	p, err := New([]string{`rm\s+-rf`}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if allowed, reason := p.Check("rm -rf /tmp/x"); allowed || reason == "" {
		t.Fatalf("allowed=%v reason=%q, want blocked", allowed, reason)
	}
	if allowed, _ := p.Check("ls -la"); !allowed {
		t.Fatal("unrelated command should not be blocked by an unrelated denylist pattern")
	}
}

func TestAllowlist_MakesPolicyDefaultDeny(t *testing.T) {
	p, err := New(nil, []string{`^ls\b`, `^cat\b`})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if allowed, _ := p.Check("ls -la"); !allowed {
		t.Fatal("ls should be allowed")
	}
	if allowed, reason := p.Check("rm -rf /"); allowed || reason == "" {
		t.Fatalf("allowed=%v reason=%q, want blocked (not on allowlist)", allowed, reason)
	}
}

func TestDenylistTakesPrecedenceOverAllowlist(t *testing.T) {
	p, err := New([]string{`rm`}, []string{`.*`}) // allow everything, but deny anything with "rm"
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if allowed, _ := p.Check("rm -rf /"); allowed {
		t.Fatal("denylist match must block even when the allowlist would otherwise permit it")
	}
	if allowed, _ := p.Check("ls -la"); !allowed {
		t.Fatal("non-denied command should pass the allow-everything allowlist")
	}
}

func TestNew_InvalidRegexReturnsError(t *testing.T) {
	if _, err := New([]string{"("}, nil); err == nil {
		t.Fatal("expected an error for an invalid deny pattern")
	}
	if _, err := New(nil, []string{"("}); err == nil {
		t.Fatal("expected an error for an invalid allow pattern")
	}
}

func TestReasonDoesNotEchoInputOrPattern(t *testing.T) {
	p, err := New([]string{`secret-pattern-xyz`}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, reason := p.Check("run secret-pattern-xyz now")
	if strings.Contains(reason, "secret-pattern-xyz") {
		t.Fatalf("reason should not echo the matched input/pattern: %q", reason)
	}
}

func TestParsePatterns(t *testing.T) {
	raw := "rm\\s+-rf\n\n# a comment\n  ^sudo\\b  \n"
	got := ParsePatterns(raw)
	want := []string{`rm\s+-rf`, `^sudo\b`}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pattern[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParsePatterns_Empty(t *testing.T) {
	if got := ParsePatterns(""); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestNew_ReDoSSafePatternsMatchQuickly(t *testing.T) {
	// A pattern shaped like a classic backtracking ReDoS trigger
	// ((a+)+b) is fine under RE2's linear-time guarantee; this mainly
	// documents the property rather than timing it precisely.
	p, err := New([]string{`(a+)+b`}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input := strings.Repeat("a", 50) + "c"
	if allowed, _ := p.Check(input); !allowed {
		t.Fatal("non-matching input should return promptly and be allowed")
	}
}
