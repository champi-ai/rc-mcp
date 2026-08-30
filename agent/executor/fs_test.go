package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFSWriteRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hello.txt")

	n, abs, err := FSWrite(path, []byte("hello world"), "overwrite", 0o644, true)
	if err != nil {
		t.Fatalf("FSWrite: %v", err)
	}
	if n != len("hello world") {
		t.Fatalf("bytesWritten = %d, want %d", n, len("hello world"))
	}
	if abs != path {
		t.Fatalf("abs = %q, want %q", abs, path)
	}

	result, err := FSRead(path, 0, 0, "")
	if err != nil {
		t.Fatalf("FSRead: %v", err)
	}
	if string(result.Content) != "hello world" {
		t.Fatalf("content = %q, want %q", result.Content, "hello world")
	}
	if result.Encoding != "utf8" {
		t.Fatalf("encoding = %q, want utf8", result.Encoding)
	}
	if result.Truncated {
		t.Fatal("should not be truncated")
	}
}

func TestFSWrite_NoCreateDirsFailsWhenParentMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-sub", "hello.txt")

	_, _, err := FSWrite(path, []byte("x"), "overwrite", 0o644, false)
	if err == nil {
		t.Fatal("expected error when parent dir missing and createDirs=false")
	}
}

func TestFSWrite_AppendMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")

	if _, _, err := FSWrite(path, []byte("a"), "overwrite", 0o644, true); err != nil {
		t.Fatalf("FSWrite(overwrite): %v", err)
	}
	if _, _, err := FSWrite(path, []byte("b"), "append", 0o644, true); err != nil {
		t.Fatalf("FSWrite(append): %v", err)
	}

	result, err := FSRead(path, 0, 0, "")
	if err != nil {
		t.Fatalf("FSRead: %v", err)
	}
	if string(result.Content) != "ab" {
		t.Fatalf("content = %q, want %q", result.Content, "ab")
	}
}

func TestFSRead_TruncatedWhenLimitHit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	if _, _, err := FSWrite(path, []byte("0123456789"), "overwrite", 0o644, true); err != nil {
		t.Fatalf("FSWrite: %v", err)
	}

	result, err := FSRead(path, 0, 4, "")
	if err != nil {
		t.Fatalf("FSRead: %v", err)
	}
	if string(result.Content) != "0123" {
		t.Fatalf("content = %q, want %q", result.Content, "0123")
	}
	if !result.Truncated {
		t.Fatal("expected truncated=true")
	}
	if result.Size != 10 {
		t.Fatalf("size = %d, want 10", result.Size)
	}
}

func TestFSRead_DirectoryIsError(t *testing.T) {
	dir := t.TempDir()
	if _, err := FSRead(dir, 0, 0, ""); err == nil {
		t.Fatal("expected error reading a directory")
	}
}

func TestFSList_RecursiveAndNonRecursive(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("b"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, ".hidden"), []byte("h"), 0o644))

	flat, err := FSList(dir, false, 0, false, 0)
	if err != nil {
		t.Fatalf("FSList(flat): %v", err)
	}
	if len(flat.Entries) != 2 { // a.txt, sub (hidden excluded)
		t.Fatalf("flat entries = %d, want 2 (got %+v)", len(flat.Entries), flat.Entries)
	}

	withHidden, err := FSList(dir, false, 0, true, 0)
	if err != nil {
		t.Fatalf("FSList(hidden): %v", err)
	}
	if len(withHidden.Entries) != 3 {
		t.Fatalf("entries with hidden = %d, want 3", len(withHidden.Entries))
	}

	rec, err := FSList(dir, true, 3, false, 0)
	if err != nil {
		t.Fatalf("FSList(recursive): %v", err)
	}
	if len(rec.Entries) != 3 { // a.txt, sub, sub/b.txt
		t.Fatalf("recursive entries = %d, want 3 (got %+v)", len(rec.Entries), rec.Entries)
	}
}

func TestFSList_Truncated(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		must(t, os.WriteFile(filepath.Join(dir, string(rune('a'+i))+".txt"), []byte("x"), 0o644))
	}

	result, err := FSList(dir, false, 0, false, 2)
	if err != nil {
		t.Fatalf("FSList: %v", err)
	}
	if len(result.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(result.Entries))
	}
	if !result.Truncated {
		t.Fatal("expected truncated=true")
	}
	if result.TotalCount != 5 {
		t.Fatalf("totalCount = %d, want 5", result.TotalCount)
	}
}

func TestFSDelete_NonEmptyDirRequiresRecursive(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	must(t, os.MkdirAll(target, 0o755))
	must(t, os.WriteFile(filepath.Join(target, "f.txt"), []byte("x"), 0o644))

	if _, err := FSDelete(target, false); err == nil {
		t.Fatal("expected error deleting non-empty dir without recursive")
	}

	result, err := FSDelete(target, true)
	if err != nil {
		t.Fatalf("FSDelete(recursive): %v", err)
	}
	if result.ItemsRemoved < 2 { // target dir + f.txt
		t.Fatalf("itemsRemoved = %d, want >= 2", result.ItemsRemoved)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("expected target to no longer exist")
	}
}

func TestFSDelete_SingleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	must(t, os.WriteFile(path, []byte("x"), 0o644))

	result, err := FSDelete(path, false)
	if err != nil {
		t.Fatalf("FSDelete: %v", err)
	}
	if result.ItemsRemoved != 1 {
		t.Fatalf("itemsRemoved = %d, want 1", result.ItemsRemoved)
	}
}

func TestFSStat_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	must(t, os.WriteFile(path, []byte("hello"), 0o644))

	result, err := FSStat(path, true)
	if err != nil {
		t.Fatalf("FSStat: %v", err)
	}
	if result.Type != "file" {
		t.Fatalf("Type = %q, want file", result.Type)
	}
	if result.Size != 5 {
		t.Fatalf("Size = %d, want 5", result.Size)
	}
	if result.Name != "f.txt" {
		t.Fatalf("Name = %q, want f.txt", result.Name)
	}
}

func TestFSStat_Symlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	must(t, os.WriteFile(target, []byte("x"), 0o644))
	must(t, os.Symlink(target, link))

	notFollowed, err := FSStat(link, false)
	if err != nil {
		t.Fatalf("FSStat(no follow): %v", err)
	}
	if notFollowed.Type != "symlink" {
		t.Fatalf("Type = %q, want symlink", notFollowed.Type)
	}
	if notFollowed.LinkTarget != target {
		t.Fatalf("LinkTarget = %q, want %q", notFollowed.LinkTarget, target)
	}

	followed, err := FSStat(link, true)
	if err != nil {
		t.Fatalf("FSStat(follow): %v", err)
	}
	if followed.Type != "file" {
		t.Fatalf("Type = %q, want file (followed)", followed.Type)
	}
}

func TestResolveAllowedPath_RejectsOutsideRoots(t *testing.T) {
	allowedDir := t.TempDir()
	outsideDir := t.TempDir()

	t.Setenv("AGENT_FS_ALLOWED_ROOTS", allowedDir)

	if _, err := resolveAllowedPath(filepath.Join(allowedDir, "ok.txt")); err != nil {
		t.Fatalf("path inside allowed root should be allowed: %v", err)
	}
	if _, err := resolveAllowedPath(filepath.Join(outsideDir, "nope.txt")); err != ErrPathNotAllowed {
		t.Fatalf("path outside allowed roots: err = %v, want ErrPathNotAllowed", err)
	}
}

func TestFSWrite_RejectsPathOutsideAllowedRoots(t *testing.T) {
	allowedDir := t.TempDir()
	outsideDir := t.TempDir()
	t.Setenv("AGENT_FS_ALLOWED_ROOTS", allowedDir)

	_, _, err := FSWrite(filepath.Join(outsideDir, "x.txt"), []byte("x"), "overwrite", 0o644, true)
	if err != ErrPathNotAllowed {
		t.Fatalf("err = %v, want ErrPathNotAllowed", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
