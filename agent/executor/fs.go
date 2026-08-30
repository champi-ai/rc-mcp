// This file implements the fs_read/fs_write/fs_list/fs_delete/fs_stat
// executors. See docs/specs/backend.md Section 3.3.
package executor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	// DefaultFSReadLimit is used when FSReadInput.Limit is unset (Section
	// 3.3.1: "default: 1048576 = 1MB").
	DefaultFSReadLimit = 1 << 20
	// FSChunkThreshold: file content at or above this size is streamed as
	// FrameFileContent binary chunks rather than inlined in the JSON
	// result (Section 3.3.1: "for large files the agent sends content as
	// binary file content frames").
	FSChunkThreshold = 256 * 1024
	// FSStreamChunkSize is the size of each streamed FrameFileContent chunk.
	FSStreamChunkSize = 64 * 1024

	// DefaultFSListMaxDepth/DefaultFSListLimit are fs_list's defaults
	// (Section 3.3.3).
	DefaultFSListMaxDepth = 3
	DefaultFSListLimit    = 1000

	// DefaultFSFileMode is used when FSWriteInput.FileMode is unset
	// (Section 3.3.2: "default: 0644").
	DefaultFSFileMode = 0o644
)

// ErrPathNotAllowed is returned when AGENT_FS_ALLOWED_ROOTS is configured
// and the requested path falls outside every configured root.
var ErrPathNotAllowed = errors.New("executor: path outside allowed roots")

// allowedRoots reads AGENT_FS_ALLOWED_ROOTS (colon-separated, like $PATH)
// each call, so tests can change the env var without needing to reset
// package state.
func allowedRoots() []string {
	v := os.Getenv("AGENT_FS_ALLOWED_ROOTS")
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, p := range filepath.SplitList(v) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

// resolveAllowedPath resolves path to an absolute path and, if
// AGENT_FS_ALLOWED_ROOTS is configured, rejects it unless it falls under one
// of those roots -- enforced here on the agent regardless of what the
// server sent (Section 12.6: defense in depth).
func resolveAllowedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("executor: resolve path: %w", err)
	}
	roots := allowedRoots()
	if len(roots) == 0 {
		return abs, nil
	}
	for _, root := range roots {
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return abs, nil
		}
	}
	return "", ErrPathNotAllowed
}

// FSReadResult is the outcome of FSRead.
type FSReadResult struct {
	Content   []byte
	Encoding  string // "utf8" | "base64"
	Size      int64
	Truncated bool
}

// FSRead reads up to limit bytes from path starting at offset. If the
// resulting content isn't valid UTF-8 and encoding wasn't explicitly
// "base64", it falls back to "base64" per Section 3.3.1.
func FSRead(path string, offset, limit int64, encoding string) (FSReadResult, error) {
	abs, err := resolveAllowedPath(path)
	if err != nil {
		return FSReadResult{}, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return FSReadResult{}, fmt.Errorf("fs_read: %w", err)
	}
	if info.IsDir() {
		return FSReadResult{}, fmt.Errorf("fs_read: %q is a directory", path)
	}

	if limit <= 0 {
		limit = DefaultFSReadLimit
	}

	f, err := os.Open(abs)
	if err != nil {
		return FSReadResult{}, fmt.Errorf("fs_read: %w", err)
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return FSReadResult{}, fmt.Errorf("fs_read: seek: %w", err)
		}
	}

	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return FSReadResult{}, fmt.Errorf("fs_read: %w", err)
	}
	data := buf[:n]

	enc := encoding
	if enc == "" {
		enc = "utf8"
	}
	if enc == "utf8" && !utf8.Valid(data) {
		enc = "base64"
	}

	return FSReadResult{
		Content:   data,
		Encoding:  enc,
		Size:      info.Size(),
		Truncated: offset+int64(n) < info.Size(),
	}, nil
}

// FSWrite writes content to path per mode ("overwrite" default, or
// "append") and fileMode, creating parent directories first iff
// createDirs.
func FSWrite(path string, content []byte, mode string, fileMode os.FileMode, createDirs bool) (bytesWritten int, absPath string, err error) {
	abs, err := resolveAllowedPath(path)
	if err != nil {
		return 0, "", err
	}

	if createDirs {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return 0, "", fmt.Errorf("fs_write: create parent dirs: %w", err)
		}
	}

	flags := os.O_WRONLY | os.O_CREATE
	if mode == "append" {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	if fileMode == 0 {
		fileMode = DefaultFSFileMode
	}

	f, err := os.OpenFile(abs, flags, fileMode)
	if err != nil {
		return 0, "", fmt.Errorf("fs_write: %w", err)
	}
	defer f.Close()

	n, err := f.Write(content)
	if err != nil {
		return n, abs, fmt.Errorf("fs_write: %w", err)
	}
	return n, abs, nil
}

// FSListResult is the outcome of FSList.
type FSListResult struct {
	Entries    []listEntry
	Truncated  bool
	TotalCount int
}

type listEntry struct {
	Name    string
	Path    string
	Type    string
	Size    int64
	Mode    string
	ModTime string
}

// FSList lists path's contents, optionally recursively up to maxDepth,
// including dotfiles iff showHidden, capped at limit entries returned
// (though TotalCount reflects how many were actually found).
func FSList(path string, recursive bool, maxDepth int, showHidden bool, limit int) (FSListResult, error) {
	abs, err := resolveAllowedPath(path)
	if err != nil {
		return FSListResult{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return FSListResult{}, fmt.Errorf("fs_list: %w", err)
	}
	if !info.IsDir() {
		return FSListResult{}, fmt.Errorf("fs_list: %q is not a directory", path)
	}
	if maxDepth <= 0 {
		maxDepth = DefaultFSListMaxDepth
	}
	if limit <= 0 {
		limit = DefaultFSListLimit
	}

	var entries []listEntry
	total := 0

	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		sort.Slice(dirEntries, func(i, j int) bool { return dirEntries[i].Name() < dirEntries[j].Name() })
		for _, de := range dirEntries {
			if !showHidden && strings.HasPrefix(de.Name(), ".") {
				continue
			}
			full := filepath.Join(dir, de.Name())
			fi, err := de.Info()
			if err != nil {
				continue
			}
			total++
			if len(entries) < limit {
				entries = append(entries, buildListEntry(de.Name(), full, fi))
			}
			if recursive && de.IsDir() && depth < maxDepth {
				_ = walk(full, depth+1)
			}
		}
		return nil
	}
	if err := walk(abs, 1); err != nil {
		return FSListResult{}, fmt.Errorf("fs_list: %w", err)
	}

	return FSListResult{Entries: entries, Truncated: total > len(entries), TotalCount: total}, nil
}

func buildListEntry(name, path string, fi fs.FileInfo) listEntry {
	return listEntry{
		Name:    name,
		Path:    path,
		Type:    fileType(fi),
		Size:    fi.Size(),
		Mode:    fi.Mode().String(),
		ModTime: fi.ModTime().UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

func fileType(fi fs.FileInfo) string {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return "symlink"
	case fi.IsDir():
		return "dir"
	case fi.Mode().IsRegular():
		return "file"
	default:
		return "other"
	}
}

// FSDeleteResult is the outcome of FSDelete.
type FSDeleteResult struct {
	ItemsRemoved int
}

// FSDelete removes path. A non-empty directory requires recursive=true; a
// file or empty directory is removed regardless.
func FSDelete(path string, recursive bool) (FSDeleteResult, error) {
	abs, err := resolveAllowedPath(path)
	if err != nil {
		return FSDeleteResult{}, err
	}

	info, err := os.Lstat(abs)
	if err != nil {
		return FSDeleteResult{}, fmt.Errorf("fs_delete: %w", err)
	}

	if !info.IsDir() {
		if err := os.Remove(abs); err != nil {
			return FSDeleteResult{}, fmt.Errorf("fs_delete: %w", err)
		}
		return FSDeleteResult{ItemsRemoved: 1}, nil
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return FSDeleteResult{}, fmt.Errorf("fs_delete: %w", err)
	}
	if len(entries) > 0 && !recursive {
		return FSDeleteResult{}, fmt.Errorf("fs_delete: %q is a non-empty directory (recursive not set)", path)
	}

	count := 0
	_ = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			count++
		}
		return nil
	})
	if err := os.RemoveAll(abs); err != nil {
		return FSDeleteResult{}, fmt.Errorf("fs_delete: %w", err)
	}
	return FSDeleteResult{ItemsRemoved: count}, nil
}

// FSStatResult is the outcome of FSStat.
type FSStatResult struct {
	Name       string
	Path       string
	Type       string
	Size       int64
	Mode       string
	ModTime    string
	Owner      string
	Group      string
	LinkTarget string
}

// FSStat returns metadata for path, following symlinks iff followSymlinks.
func FSStat(path string, followSymlinks bool) (FSStatResult, error) {
	abs, err := resolveAllowedPath(path)
	if err != nil {
		return FSStatResult{}, err
	}

	var info os.FileInfo
	var linkTarget string
	if followSymlinks {
		info, err = os.Stat(abs)
	} else {
		info, err = os.Lstat(abs)
		if err == nil && info.Mode()&fs.ModeSymlink != 0 {
			linkTarget, _ = os.Readlink(abs)
		}
	}
	if err != nil {
		return FSStatResult{}, fmt.Errorf("fs_stat: %w", err)
	}

	result := FSStatResult{
		Name:       filepath.Base(abs),
		Path:       abs,
		Type:       fileType(info),
		Size:       info.Size(),
		Mode:       info.Mode().String(),
		ModTime:    info.ModTime().UTC().Format("2006-01-02T15:04:05Z07:00"),
		LinkTarget: linkTarget,
	}

	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if u, err := user.LookupId(strconv.FormatUint(uint64(stat.Uid), 10)); err == nil {
			result.Owner = u.Username
		}
		if g, err := user.LookupGroupId(strconv.FormatUint(uint64(stat.Gid), 10)); err == nil {
			result.Group = g.Name
		}
	}

	return result, nil
}
