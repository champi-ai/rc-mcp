package types

// ProcessListInput is the input schema for process_list (docs/specs/backend.md
// Section 3.4.1).
type ProcessListInput struct {
	ClientID string  `json:"clientId"`
	Filter   *string `json:"filter,omitempty"`
	User     *string `json:"user,omitempty"`
	SortBy   *string `json:"sortBy,omitempty"` // pid | cpu | memory | name
	Limit    *int    `json:"limit,omitempty"`
}

// ProcessSummary is one entry in process_list's output.
type ProcessSummary struct {
	PID       int     `json:"pid"`
	PPID      int     `json:"ppid"`
	Name      string  `json:"name"`
	Cmdline   string  `json:"cmdline"`
	User      string  `json:"user"`
	CPUPct    float64 `json:"cpuPct"`
	MemPct    float64 `json:"memPct"`
	MemRSSKB  int64   `json:"memRssKB"`
	State     string  `json:"state"`
	StartTime string  `json:"startTime"`
}

// ProcessListOutput is the output schema for process_list.
type ProcessListOutput struct {
	Processes  []ProcessSummary `json:"processes"`
	TotalCount int              `json:"totalCount"`
	ClientID   string           `json:"clientId"`
}

// ProcessInfoInput is the input schema for process_info (Section 3.4.2).
type ProcessInfoInput struct {
	ClientID string `json:"clientId"`
	PID      int    `json:"pid"`
}

// ProcessInfoOutput is the output schema for process_info.
type ProcessInfoOutput struct {
	PID       int               `json:"pid"`
	PPID      int               `json:"ppid"`
	Name      string            `json:"name"`
	Cmdline   string            `json:"cmdline"`
	Exe       string            `json:"exe,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	User      string            `json:"user"`
	State     string            `json:"state"`
	Threads   int               `json:"threads"`
	CPUPct    float64           `json:"cpuPct"`
	MemPct    float64           `json:"memPct"`
	MemRSSKB  int64             `json:"memRssKB"`
	MemVMSKB  int64             `json:"memVmsKB"`
	StartTime string            `json:"startTime"`
	FDs       int               `json:"fds,omitempty"`
	Environ   map[string]string `json:"environ,omitempty"`
	ClientID  string            `json:"clientId"`
}

// ProcessSignalInput is the input schema for process_signal (Section 3.4.3).
type ProcessSignalInput struct {
	ClientID string  `json:"clientId"`
	PID      int     `json:"pid"`
	Signal   *string `json:"signal,omitempty"` // default SIGTERM
}

// ProcessSignalOutput is the output schema for process_signal.
type ProcessSignalOutput struct {
	SignalSent bool   `json:"signalSent"`
	PID        int    `json:"pid"`
	Signal     string `json:"signal"`
	ClientID   string `json:"clientId"`
}
