package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShouldUpdate(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"1.0.0", "1.1.0", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "", false},
		{"1.0.0", "dev", false},
		{"dev", "1.1.0", false},
	}
	for _, c := range cases {
		if got := ShouldUpdate(c.current, c.latest); got != c.want {
			t.Errorf("ShouldUpdate(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestReleaseURLs(t *testing.T) {
	cfg := Config{BaseURL: "https://example.invalid/releases/download", GOOS: "linux", GOARCH: "arm64"}
	bin, sum := cfg.releaseURLs("1.2.3")
	wantBin := "https://example.invalid/releases/download/agent-v1.2.3/rc-mcp-agent-linux-arm64"
	if bin != wantBin {
		t.Errorf("binaryURL = %q, want %q", bin, wantBin)
	}
	if sum != wantBin+".sha256" {
		t.Errorf("checksumURL = %q, want %q", sum, wantBin+".sha256")
	}
}

func TestReleaseURLs_TrimsTrailingSlash(t *testing.T) {
	cfg := Config{BaseURL: "https://example.invalid/releases/download/", GOOS: "linux", GOARCH: "amd64"}
	bin, _ := cfg.releaseURLs("1.0.0")
	if strings.Contains(bin, "download//") {
		t.Errorf("binaryURL has a double slash: %q", bin)
	}
}

func TestParseChecksumFile(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	got, err := parseChecksumFile([]byte(digest + "  rc-mcp-agent-linux-amd64\n"))
	if err != nil {
		t.Fatalf("parseChecksumFile: %v", err)
	}
	if got != digest {
		t.Errorf("got %q, want %q", got, digest)
	}
}

func TestParseChecksumFile_Malformed(t *testing.T) {
	for _, in := range []string{"", "not-a-digest  file", "\n\n"} {
		if _, err := parseChecksumFile([]byte(in)); err == nil {
			t.Errorf("input %q: expected an error", in)
		}
	}
}

// testServer serves a binary and its matching checksum at the URL pattern
// Update expects.
func testServer(t *testing.T, binary []byte, corruptChecksum bool) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(binary)
	digest := hex.EncodeToString(sum[:])
	if corruptChecksum {
		digest = strings.Repeat("00", 32)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/agent-v1.2.3/rc-mcp-agent-linux-amd64", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(binary)
	})
	mux.HandleFunc("/agent-v1.2.3/rc-mcp-agent-linux-amd64.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  rc-mcp-agent-linux-amd64\n", digest)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func withFakeRestart(t *testing.T) *bool {
	t.Helper()
	called := false
	origReplace, origRestart := replaceSelfFn, restartFn
	restartFn = func(unit, execPath string) { called = true }
	t.Cleanup(func() { replaceSelfFn, restartFn = origReplace, origRestart })
	return &called
}

func TestUpdate_Success_ReplacesBinaryAndRestarts(t *testing.T) {
	binary := []byte("new-binary-contents-v1.2.3")
	srv := testServer(t, binary, false)
	restarted := withFakeRestart(t)

	execPath := filepath.Join(t.TempDir(), "rc-mcp-agent")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("seed old binary: %v", err)
	}

	cfg := Config{BaseURL: srv.URL, GOOS: "linux", GOARCH: "amd64"}
	if err := Update(context.Background(), cfg, "1.2.3", execPath); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(binary) {
		t.Fatalf("binary contents = %q, want %q", got, binary)
	}
	fi, err := os.Stat(execPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatal("replaced binary should be executable")
	}
	if !*restarted {
		t.Fatal("Update should call restartFn on success")
	}
}

func TestUpdate_ChecksumMismatch_RefusesToInstall(t *testing.T) {
	binary := []byte("new-binary-contents")
	srv := testServer(t, binary, true) // corrupted checksum
	restarted := withFakeRestart(t)

	execPath := filepath.Join(t.TempDir(), "rc-mcp-agent")
	original := []byte("original-binary")
	if err := os.WriteFile(execPath, original, 0o755); err != nil {
		t.Fatalf("seed original binary: %v", err)
	}

	cfg := Config{BaseURL: srv.URL, GOOS: "linux", GOARCH: "amd64"}
	err := Update(context.Background(), cfg, "1.2.3", execPath)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}

	got, _ := os.ReadFile(execPath)
	if string(got) != string(original) {
		t.Fatal("the original binary must be untouched after a checksum failure")
	}
	if *restarted {
		t.Fatal("a failed update must never trigger a restart")
	}
}

func TestUpdate_BinaryFetchFails_NoInstall(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent-v1.2.3/rc-mcp-agent-linux-amd64.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  x\n", strings.Repeat("ab", 32))
	})
	mux.HandleFunc("/agent-v1.2.3/rc-mcp-agent-linux-amd64", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	restarted := withFakeRestart(t)

	execPath := filepath.Join(t.TempDir(), "rc-mcp-agent")
	if err := os.WriteFile(execPath, []byte("original"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cfg := Config{BaseURL: srv.URL, GOOS: "linux", GOARCH: "amd64"}
	if err := Update(context.Background(), cfg, "1.2.3", execPath); err == nil {
		t.Fatal("expected an error when the binary fetch fails")
	}
	if *restarted {
		t.Fatal("a failed fetch must never trigger a restart")
	}
}

func TestUpdate_ChecksumFetchFails_NoInstall(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent-v1.2.3/rc-mcp-agent-linux-amd64.sha256", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	restarted := withFakeRestart(t)

	execPath := filepath.Join(t.TempDir(), "rc-mcp-agent")
	if err := os.WriteFile(execPath, []byte("original"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := Config{BaseURL: srv.URL, GOOS: "linux", GOARCH: "amd64"}
	if err := Update(context.Background(), cfg, "1.2.3", execPath); err == nil {
		t.Fatal("expected an error when the checksum fetch fails")
	}
	if *restarted {
		t.Fatal("a failed fetch must never trigger a restart")
	}
}

func TestRestart_FallsBackToExecOnSystemctlFailure(t *testing.T) {
	origSystemctl, origExec := systemctlRestartFn, execFn
	defer func() { systemctlRestartFn, execFn = origSystemctl, origExec }()

	systemctlRestartFn = func(unit string) error { return errors.New("no systemd here") }
	execCalled := false
	execFn = func(argv0 string, argv, envv []string) error {
		execCalled = true
		return nil
	}

	restart("rc-mcp-agent", "/usr/local/bin/rc-mcp-agent")
	if !execCalled {
		t.Fatal("restart should fall back to execFn when systemctl fails")
	}
}

func TestRestart_SkipsExecFallbackOnSystemctlSuccess(t *testing.T) {
	origSystemctl, origExec := systemctlRestartFn, execFn
	defer func() { systemctlRestartFn, execFn = origSystemctl, origExec }()

	systemctlRestartFn = func(unit string) error { return nil }
	execCalled := false
	execFn = func(argv0 string, argv, envv []string) error {
		execCalled = true
		return nil
	}

	restart("rc-mcp-agent", "/usr/local/bin/rc-mcp-agent")
	if execCalled {
		t.Fatal("restart should not fall back to execFn when systemctl succeeds")
	}
}

func TestReplaceSelf_AtomicAndExecutable(t *testing.T) {
	execPath := filepath.Join(t.TempDir(), "rc-mcp-agent")
	if err := os.WriteFile(execPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := replaceSelf([]byte("new-contents"), execPath); err != nil {
		t.Fatalf("replaceSelf: %v", err)
	}
	got, err := os.ReadFile(execPath)
	if err != nil || string(got) != "new-contents" {
		t.Fatalf("got = %q, err = %v", got, err)
	}
	fi, _ := os.Stat(execPath)
	if fi.Mode()&0o111 == 0 {
		t.Fatal("replaced file should be executable")
	}

	// No leftover temp file.
	entries, _ := os.ReadDir(filepath.Dir(execPath))
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries, want exactly the replaced binary: %v", len(entries), entries)
	}
}

func TestDefaultArch(t *testing.T) {
	// Just exercises the branch without asserting a specific value, since
	// the result depends on the test runner's GOARCH.
	if got := defaultArch(); got != "amd64" && got != "arm64" {
		t.Fatalf("defaultArch() = %q, want amd64 or arm64", got)
	}
}
