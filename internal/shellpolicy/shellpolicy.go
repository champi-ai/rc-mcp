// Package shellpolicy implements operator-configurable shell command
// allowlist/denylist enforcement, evaluated server-side before any
// dispatch to an agent (docs/specs/backend.md Section 19, phase-3-post-mvp
// Risks "ReDoS"). Patterns are compiled with Go's regexp package (RE2,
// guaranteed linear-time matching) -- never a backtracking engine -- so an
// operator-supplied pattern can never cause catastrophic backtracking.
package shellpolicy

import (
	"fmt"
	"regexp"
	"strings"
)

// Policy holds compiled denylist and allowlist patterns and decides
// whether a command/input string may be dispatched.
//
// Evaluation order: a denylist match always blocks, regardless of the
// allowlist. If the allowlist is non-empty, the input must match at least
// one allowlist pattern to proceed (a non-empty allowlist makes the policy
// default-deny). An empty policy (no patterns configured either way, the
// MVP default) allows everything, matching current unrestricted behavior.
type Policy struct {
	deny  []*regexp.Regexp
	allow []*regexp.Regexp
}

// New compiles denyPatterns and allowPatterns (each a list of regular
// expressions) into a Policy. A nil/empty Policy (both lists empty) is
// valid and allows everything. Returns an error naming the first pattern
// that fails to compile as RE2.
func New(denyPatterns, allowPatterns []string) (*Policy, error) {
	deny, err := compileAll("deny", denyPatterns)
	if err != nil {
		return nil, err
	}
	allow, err := compileAll("allow", allowPatterns)
	if err != nil {
		return nil, err
	}
	return &Policy{deny: deny, allow: allow}, nil
}

func compileAll(kind string, patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("shellpolicy: invalid %s pattern %q: %w", kind, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// Check reports whether input may be dispatched. When ok is false, reason
// explains why (suitable for a tool error message, without echoing the
// input back or naming which specific pattern matched -- an operator's
// denylist patterns are not meant to be discoverable by probing).
func (p *Policy) Check(input string) (ok bool, reason string) {
	if p == nil {
		return true, ""
	}
	for _, re := range p.deny {
		if re.MatchString(input) {
			return false, "blocked by server denylist policy"
		}
	}
	if len(p.allow) > 0 {
		for _, re := range p.allow {
			if re.MatchString(input) {
				return true, ""
			}
		}
		return false, "not permitted by server allowlist policy"
	}
	return true, ""
}

// ParsePatterns splits an operator-supplied env var value into individual
// regex patterns, one per newline, ignoring blank lines and lines starting
// with "#" (comments). This is the format RC_SHELL_ALLOWLIST and
// RC_SHELL_DENYLIST use: a comma-based delimiter would collide with
// commas legitimately appearing inside a pattern (e.g. `{2,4}`).
func ParsePatterns(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
