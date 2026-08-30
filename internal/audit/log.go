package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Logger is an append-only writer for audit.Entry records, one per line as
// newline-delimited JSON. It detects external log rotation (a rename +
// fresh file created at the same path, e.g. by logrotate) by inode and
// reopens automatically, per Section 12.7.
type Logger struct {
	path string

	// OnWrite, if set, is invoked after each successfully appended entry
	// (outside the logger's lock). Used to push audit://log resource
	// update notifications (Section 4.4). Set before first use; not
	// synchronized with concurrent writes.
	OnWrite func(Entry)

	// FullArgs opts into forensic full-argument logging
	// (RC_AUDIT_FULL_ARGS=true, Section 12.7): LogCall attaches the
	// complete, unredacted arguments to each Entry instead of only
	// ArgsDigest/ArgsHint. Off by default. Set before first use; not
	// synchronized with concurrent writes.
	FullArgs bool

	mu    sync.Mutex
	file  *os.File
	inode uint64
}

// NewLogger opens (creating if necessary) the audit log at path for
// appending. The parent directory is created if it does not exist.
func NewLogger(path string) (*Logger, error) {
	l := &Logger{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Logger) open() error {
	dir := filepath.Dir(l.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("audit: create log directory: %w", err)
		}
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("audit: open log file: %w", err)
	}
	l.file = f
	l.inode = inodeOf(f)
	return nil
}

// reopenLocked closes the current file handle (if any) and reopens (or
// creates) the file at l.path. Caller must hold l.mu.
func (l *Logger) reopenLocked() error {
	if l.file != nil {
		_ = l.file.Close()
	}
	return l.open()
}

// rotatedLocked reports whether the file currently at l.path is not the
// same file l.file points to (external rotation). Caller must hold l.mu.
func (l *Logger) rotatedLocked() bool {
	fi, err := os.Stat(l.path)
	if err != nil {
		// Missing (renamed away and not yet recreated) counts as rotated;
		// reopen will recreate it with O_CREATE.
		return true
	}
	return inodeOfFileInfo(fi) != l.inode
}

// Write appends entry as a single line of JSON to the log, reopening the
// underlying file first if it detects the file at path was externally
// rotated since the last write.
func (l *Logger) Write(entry Entry) error {
	l.mu.Lock()

	if l.rotatedLocked() {
		if err := l.reopenLocked(); err != nil {
			l.mu.Unlock()
			return err
		}
	}

	data, err := json.Marshal(entry)
	if err != nil {
		l.mu.Unlock()
		return fmt.Errorf("audit: marshal entry: %w", err)
	}
	data = append(data, '\n')

	if _, err := l.file.Write(data); err != nil {
		l.mu.Unlock()
		return fmt.Errorf("audit: write entry: %w", err)
	}
	l.mu.Unlock()

	if l.OnWrite != nil {
		l.OnWrite(entry)
	}
	return nil
}

// LogCall is a convenience wrapper around Write for the common case of
// logging one tool invocation.
func (l *Logger) LogCall(sessionID, clientID, tool string, args any, status string, duration time.Duration, errMsg string) error {
	entry := Entry{
		Timestamp:  time.Now().UTC(),
		SessionID:  sessionID,
		ClientID:   clientID,
		Tool:       tool,
		ArgsDigest: DigestArgs(args),
		ArgsHint:   Hint(args),
		Status:     status,
		DurationMs: duration.Milliseconds(),
		Error:      errMsg,
	}
	if l.FullArgs {
		if raw, err := json.Marshal(args); err == nil {
			entry.FullArgs = raw
		}
	}
	return l.Write(entry)
}

// Close closes the underlying file handle.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	return l.file.Close()
}

// DigestArgs returns the hex-encoded SHA-256 digest of the JSON encoding
// of args. Raw args are never logged (Section 12.7).
func DigestArgs(args any) string {
	data, err := json.Marshal(args)
	if err != nil {
		data = []byte("null")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sensitiveHintKeys lists field names redacted from Hint's output because
// they commonly carry bulk/sensitive payload data rather than a short,
// human-scannable value (Section 12.7: e.g. shell_session_write's input is
// logged as a digest only, never raw).
var sensitiveHintKeys = map[string]bool{
	"stdin":    true,
	"input":    true,
	"content":  true,
	"password": true,
	"token":    true,
	"secret":   true,
}

// Hint builds a short, sanitized summary of args suitable for quick log
// scanning (e.g. "command=ls -la, clientId=dev-1"), from either a
// map[string]any or any JSON-marshalable struct. Fields in
// sensitiveHintKeys are redacted; ArgsDigest remains the authoritative
// record of the full args.
func Hint(args any) string {
	var m map[string]any
	data, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	for k := range m {
		if sensitiveHintKeys[strings.ToLower(k)] {
			m[k] = "[redacted]"
		}
	}
	return HintFromMap(m, defaultHintMaxLen)
}

const defaultHintMaxLen = 200

// HintFromMap joins map entries as "key=value" pairs, sorted by key, and
// truncates to maxLen (0 = unlimited).
func HintFromMap(fields map[string]any, maxLen int) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, fields[k]))
	}
	s := strings.Join(parts, ", ")
	if maxLen > 0 && len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}

func inodeOf(f *os.File) uint64 {
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	return inodeOfFileInfo(fi)
}

func inodeOfFileInfo(fi os.FileInfo) uint64 {
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
		return stat.Ino
	}
	return 0
}
