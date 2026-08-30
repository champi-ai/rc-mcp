// Package types holds the MCP tool input/output types shared by the
// server's tool handlers and (for the fields the agent-side executors also
// need) the desktop agent. See docs/specs/backend.md Section 6.
package types

// ShellExecInput is the input schema for the shell_exec tool.
type ShellExecInput struct {
	ClientID string            `json:"clientId"`
	Command  string            `json:"command"`
	Cwd      *string           `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Timeout  *int              `json:"timeout,omitempty"` // seconds
	Stdin    *string           `json:"stdin,omitempty"`
}

// ShellExecOutput is the output schema for the shell_exec tool.
type ShellExecOutput struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	Killed     bool   `json:"killed"`
	DurationMs int64  `json:"durationMs"`
	ClientID   string `json:"clientId"`
}
