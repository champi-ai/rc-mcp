package fsroot

import "testing"

func TestNew_EmptyAllowsEverything(t *testing.T) {
	p, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if allowed, reason := p.Check("/etc/shadow"); !allowed || reason != "" {
		t.Fatalf("allowed=%v reason=%q, want allowed with no configured roots", allowed, reason)
	}
}

func TestCheck_NilReceiverAllowsEverything(t *testing.T) {
	var p *Policy
	if allowed, _ := p.Check("/anything"); !allowed {
		t.Fatal("a nil *Policy must allow everything")
	}
}

func TestCheck_AbsolutePathInsideRoot(t *testing.T) {
	p, err := New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if allowed, _ := p.Check("/srv/data/file.txt"); !allowed {
		t.Fatal("path under an allowed root should be allowed")
	}
	if allowed, _ := p.Check("/srv/data"); !allowed {
		t.Fatal("the root itself should be allowed")
	}
}

func TestCheck_AbsolutePathOutsideRoot(t *testing.T) {
	p, err := New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if allowed, reason := p.Check("/etc/shadow"); allowed || reason == "" {
		t.Fatalf("allowed=%v reason=%q, want blocked", allowed, reason)
	}
}

func TestCheck_RejectsPrefixLookalike(t *testing.T) {
	// "/srv/data-other" must not be treated as under "/srv/data".
	p, err := New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if allowed, _ := p.Check("/srv/data-other/file.txt"); allowed {
		t.Fatal("a sibling directory sharing a string prefix must not be allowed")
	}
}

func TestCheck_CleansDotDotWithinRoot(t *testing.T) {
	p, err := New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if allowed, _ := p.Check("/srv/data/sub/../file.txt"); !allowed {
		t.Fatal("a cleaned path still under the root should be allowed")
	}
	if allowed, _ := p.Check("/srv/data/../secrets"); allowed {
		t.Fatal("a cleaned path escaping the root via .. must be blocked")
	}
}

func TestCheck_RelativePathAlwaysPasses(t *testing.T) {
	p, err := New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The server has no notion of the target agent's cwd; relative paths
	// are left to the agent's own AGENT_FS_ALLOWED_ROOTS.
	if allowed, _ := p.Check("relative/path.txt"); !allowed {
		t.Fatal("a relative path must pass the global check unconditionally")
	}
}

func TestNew_RejectsRelativeRoot(t *testing.T) {
	if _, err := New([]string{"relative/root"}); err == nil {
		t.Fatal("expected an error for a non-absolute configured root")
	}
}

func TestParseRoots(t *testing.T) {
	got := ParseRoots("/srv/data:/var/app:  /tmp/scratch  ")
	want := []string{"/srv/data", "/var/app", "/tmp/scratch"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("root[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseRoots_Empty(t *testing.T) {
	if got := ParseRoots(""); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}
