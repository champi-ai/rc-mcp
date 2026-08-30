// Package fsroot implements the server-side global filesystem root policy
// (RC_GLOBAL_FS_ALLOWED_ROOTS, docs/specs/backend.md Section 12.6): a
// coarse, server-side check applied before dispatch, in addition to (not
// instead of) each agent's own AGENT_FS_ALLOWED_ROOTS.
package fsroot

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Policy holds the configured global roots and decides whether a path may
// be dispatched to an fs_* tool.
//
// The server has no meaningful notion of "current directory" on the
// target agent's machine, so this check only applies to absolute paths --
// a relative path is not evaluated here (it passes through unchecked) and
// relies entirely on the target agent's own AGENT_FS_ALLOWED_ROOTS, which
// resolves it relative to that agent's own working directory. An empty
// Policy (the default) allows everything.
type Policy struct {
	roots []string
}

// New compiles roots (each must be an absolute path) into a Policy. A
// nil/empty roots list is valid and allows everything.
func New(roots []string) (*Policy, error) {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("fsroot: root %q must be an absolute path", r)
		}
		out = append(out, filepath.Clean(r))
	}
	return &Policy{roots: out}, nil
}

// Check reports whether path may be dispatched. Relative paths always
// pass (see the Policy doc comment); an absolute path must fall under one
// of the configured roots when any are configured.
func (p *Policy) Check(path string) (ok bool, reason string) {
	if p == nil || len(p.roots) == 0 {
		return true, ""
	}
	if !filepath.IsAbs(path) {
		return true, ""
	}
	clean := filepath.Clean(path)
	for _, root := range p.roots {
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true, ""
		}
	}
	return false, "outside the server's globally allowed filesystem roots"
}

// ParseRoots splits an operator-supplied RC_GLOBAL_FS_ALLOWED_ROOTS value
// (colon-separated, like $PATH -- matching AGENT_FS_ALLOWED_ROOTS'
// convention) into individual root strings, trimming whitespace and
// dropping empty entries.
func ParseRoots(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, r := range filepath.SplitList(raw) {
		r = strings.TrimSpace(r)
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}
