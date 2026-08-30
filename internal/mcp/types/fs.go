package types

// FSReadInput is the input schema for fs_read (docs/specs/backend.md
// Section 3.3.1).
type FSReadInput struct {
	ClientID string  `json:"clientId"`
	Path     string  `json:"path"`
	Offset   *int64  `json:"offset,omitempty"`
	Limit    *int64  `json:"limit,omitempty"`
	Encoding *string `json:"encoding,omitempty"` // "utf8" | "base64"
}

// FSReadOutput is the output schema for fs_read.
type FSReadOutput struct {
	Content   string `json:"content"`
	Encoding  string `json:"encoding"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	ClientID  string `json:"clientId"`
}

// FSWriteInput is the input schema for fs_write (Section 3.3.2).
type FSWriteInput struct {
	ClientID   string  `json:"clientId"`
	Path       string  `json:"path"`
	Content    string  `json:"content"`
	Encoding   *string `json:"encoding,omitempty"` // "utf8" | "base64", default utf8
	Mode       *string `json:"mode,omitempty"`     // "overwrite" | "append", default overwrite
	FileMode   *string `json:"fileMode,omitempty"` // octal string, default "0644"
	CreateDirs *bool   `json:"createDirs,omitempty"`
}

// FSWriteOutput is the output schema for fs_write.
type FSWriteOutput struct {
	BytesWritten int    `json:"bytesWritten"`
	Path         string `json:"path"`
	ClientID     string `json:"clientId"`
}

// FSListInput is the input schema for fs_list (Section 3.3.3).
type FSListInput struct {
	ClientID   string `json:"clientId"`
	Path       string `json:"path"`
	Recursive  *bool  `json:"recursive,omitempty"`
	MaxDepth   *int   `json:"maxDepth,omitempty"`
	ShowHidden *bool  `json:"showHidden,omitempty"`
	Limit      *int   `json:"limit,omitempty"`
}

// FSEntry is one entry in fs_list's output.
type FSEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"` // "file" | "dir" | "symlink" | "other"
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"modTime"`
}

// FSListOutput is the output schema for fs_list.
type FSListOutput struct {
	Entries    []FSEntry `json:"entries"`
	Truncated  bool      `json:"truncated"`
	TotalCount int       `json:"totalCount"`
	ClientID   string    `json:"clientId"`
}

// FSDeleteInput is the input schema for fs_delete (Section 3.3.4).
type FSDeleteInput struct {
	ClientID  string `json:"clientId"`
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive,omitempty"`
}

// FSDeleteOutput is the output schema for fs_delete.
type FSDeleteOutput struct {
	Deleted      bool   `json:"deleted"`
	Path         string `json:"path"`
	ItemsRemoved int    `json:"itemsRemoved"`
	ClientID     string `json:"clientId"`
}

// FSStatInput is the input schema for fs_stat (Section 3.3.5).
type FSStatInput struct {
	ClientID       string `json:"clientId"`
	Path           string `json:"path"`
	FollowSymlinks *bool  `json:"followSymlinks,omitempty"`
}

// FSStatOutput is the output schema for fs_stat.
type FSStatOutput struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	Mode       string `json:"mode"`
	ModTime    string `json:"modTime"`
	Owner      string `json:"owner,omitempty"`
	Group      string `json:"group,omitempty"`
	LinkTarget string `json:"linkTarget,omitempty"`
	ClientID   string `json:"clientId"`
}
