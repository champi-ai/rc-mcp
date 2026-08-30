package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
)

// ReadEntries reads the audit log at path and returns entries newest-first,
// skipping offset entries and returning at most limit (limit <= 0 means no
// limit), per the audit://log resource contract (Section 4.4). Lines that
// fail to parse are skipped rather than failing the whole read. A missing
// log file yields an empty slice.
func ReadEntries(path string, limit, offset int) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Entry{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var all []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		all = append(all, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// The file is append-order (oldest first); reverse to newest-first.
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}

	if offset < 0 {
		offset = 0
	}
	if offset >= len(all) {
		return []Entry{}, nil
	}
	all = all[offset:]
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
