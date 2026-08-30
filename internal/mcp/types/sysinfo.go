package types

// SysinfoGetInput is the input schema for sysinfo_get (docs/specs/backend.md
// Section 3.5.1).
type SysinfoGetInput struct {
	ClientID string   `json:"clientId"`
	Sections []string `json:"sections,omitempty"` // default: ["all"]
}

// SysinfoOS is the "os" section.
type SysinfoOS struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Kernel  string `json:"kernel,omitempty"`
	Arch    string `json:"arch,omitempty"`
}

// SysinfoUptime is the "uptime" section.
type SysinfoUptime struct {
	Seconds int64  `json:"seconds"`
	Human   string `json:"human"`
}

// SysinfoCPU is the "cpu" section.
type SysinfoCPU struct {
	Model     string  `json:"model,omitempty"`
	Cores     int     `json:"cores,omitempty"`
	Threads   int     `json:"threads,omitempty"`
	UsagePct  float64 `json:"usagePct"`
	LoadAvg1  float64 `json:"loadAvg1"`
	LoadAvg5  float64 `json:"loadAvg5"`
	LoadAvg15 float64 `json:"loadAvg15"`
}

// SysinfoMemory is the "memory" section.
type SysinfoMemory struct {
	TotalKB     int64   `json:"totalKB"`
	UsedKB      int64   `json:"usedKB"`
	AvailableKB int64   `json:"availableKB"`
	UsagePct    float64 `json:"usagePct"`
	SwapTotalKB int64   `json:"swapTotalKB"`
	SwapUsedKB  int64   `json:"swapUsedKB"`
}

// SysinfoDisk is one entry in the "disk" section.
type SysinfoDisk struct {
	Mount       string  `json:"mount"`
	Device      string  `json:"device"`
	FSType      string  `json:"fsType"`
	TotalKB     int64   `json:"totalKB"`
	UsedKB      int64   `json:"usedKB"`
	AvailableKB int64   `json:"availableKB"`
	UsagePct    float64 `json:"usagePct"`
}

// SysinfoNetworkIface is one entry in the "network" section.
type SysinfoNetworkIface struct {
	Name  string `json:"name"`
	IPv4  string `json:"ipv4,omitempty"`
	IPv6  string `json:"ipv6,omitempty"`
	MAC   string `json:"mac,omitempty"`
	State string `json:"state"`
}

// SysinfoGetOutput is the output schema for sysinfo_get. Every section
// pointer is nil if not requested or unavailable on the agent (Section
// 3.5.1: "returns partial result with null sections, not an error").
type SysinfoGetOutput struct {
	Hostname string                `json:"hostname,omitempty"`
	OS       *SysinfoOS            `json:"os,omitempty"`
	Uptime   *SysinfoUptime        `json:"uptime,omitempty"`
	CPU      *SysinfoCPU           `json:"cpu,omitempty"`
	Memory   *SysinfoMemory        `json:"memory,omitempty"`
	Disk     []SysinfoDisk         `json:"disk,omitempty"`
	Network  []SysinfoNetworkIface `json:"network,omitempty"`
	ClientID string                `json:"clientId"`
}
