package types

// ShellSessionStartInput is the input schema for shell_session_start
// (docs/specs/backend.md Section 3.1.2).
type ShellSessionStartInput struct {
	ClientID string            `json:"clientId"`
	Shell    *string           `json:"shell,omitempty"`
	Cwd      *string           `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Rows     *int              `json:"rows,omitempty"`
	Cols     *int              `json:"cols,omitempty"`
}

// ShellSessionStartOutput is the output schema for shell_session_start.
type ShellSessionStartOutput struct {
	ShellSessionID string `json:"shellSessionId"`
	PID            int    `json:"pid"`
	Shell          string `json:"shell"`
	ClientID       string `json:"clientId"`
}

// ShellSessionWriteInput is the input schema for shell_session_write
// (Section 3.1.3).
type ShellSessionWriteInput struct {
	ShellSessionID string `json:"shellSessionId"`
	Input          string `json:"input"`
}

// ShellSessionWriteOutput is the output schema for shell_session_write.
type ShellSessionWriteOutput struct {
	BytesWritten int    `json:"bytesWritten"`
	Output       string `json:"output,omitempty"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	Exited       bool   `json:"exited,omitempty"`
}

// ShellSessionCloseInput is the input schema for shell_session_close
// (Section 3.1.4).
type ShellSessionCloseInput struct {
	ShellSessionID string  `json:"shellSessionId"`
	Signal         *string `json:"signal,omitempty"` // "SIGTERM" or "SIGKILL"
}

// ShellSessionCloseOutput is the output schema for shell_session_close.
type ShellSessionCloseOutput struct {
	ExitCode    int    `json:"exitCode"`
	FinalOutput string `json:"finalOutput,omitempty"`
}
