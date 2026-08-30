// This file implements the sysinfo_get executor. See docs/specs/backend.md
// Section 3.5.1. Every gather* function degrades gracefully -- an
// unreadable subsystem yields a nil/zero-value result rather than an error,
// so one broken section never fails the whole call.
package executor

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SysinfoResult mirrors types.SysinfoGetOutput without importing the mcp
// types package's JSON tags, so this file stays agent-only.
type SysinfoResult struct {
	Hostname string
	OS       *SysinfoOS
	Uptime   *SysinfoUptime
	CPU      *SysinfoCPU
	Memory   *SysinfoMemory
	Disk     []SysinfoDisk
	Network  []SysinfoNetworkIface
}

type SysinfoOS struct{ Name, Version, Kernel, Arch string }
type SysinfoUptime struct {
	Seconds int64
	Human   string
}
type SysinfoCPU struct {
	Model                         string
	Cores, Threads                int
	UsagePct                      float64
	LoadAvg1, LoadAvg5, LoadAvg15 float64
}
type SysinfoMemory struct {
	TotalKB, UsedKB, AvailableKB, SwapTotalKB, SwapUsedKB int64
	UsagePct                                              float64
}
type SysinfoDisk struct {
	Mount, Device, FSType        string
	TotalKB, UsedKB, AvailableKB int64
	UsagePct                     float64
}
type SysinfoNetworkIface struct {
	Name, IPv4, IPv6, MAC, State string
}

// GatherSysinfo collects the requested sections (case-insensitive; "all" or
// an empty slice means every section).
func GatherSysinfo(sections []string) SysinfoResult {
	want := sectionSet(sections)

	var result SysinfoResult
	if want["hostname"] {
		if h, err := os.Hostname(); err == nil {
			result.Hostname = h
		}
	}
	if want["os"] {
		result.OS = gatherOS()
	}
	if want["uptime"] {
		result.Uptime = gatherUptime()
	}
	if want["cpu"] {
		result.CPU = gatherCPU()
	}
	if want["memory"] {
		result.Memory = gatherMemory()
	}
	if want["disk"] {
		result.Disk = gatherDisk()
	}
	if want["network"] {
		result.Network = gatherNetwork()
	}
	return result
}

func sectionSet(sections []string) map[string]bool {
	all := map[string]bool{
		"hostname": true, "os": true, "uptime": true, "cpu": true,
		"memory": true, "disk": true, "network": true,
	}
	if len(sections) == 0 {
		return all
	}
	set := map[string]bool{}
	for _, s := range sections {
		if strings.EqualFold(s, "all") {
			return all
		}
		set[strings.ToLower(s)] = true
	}
	if len(set) == 0 {
		return all
	}
	return set
}

func gatherOS() *SysinfoOS {
	info := &SysinfoOS{Arch: runtime.GOARCH}
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				info.Name = strings.Trim(v, `"`)
			} else if v, ok := strings.CutPrefix(line, "VERSION_ID="); ok {
				info.Version = strings.Trim(v, `"`)
			}
		}
	}
	var uname syscall.Utsname
	if err := syscall.Uname(&uname); err == nil {
		info.Kernel = utsnameToString(uname.Release[:])
		if info.Name == "" {
			info.Name = utsnameToString(uname.Sysname[:])
		}
	}
	if info.Name == "" && info.Kernel == "" {
		return nil
	}
	return info
}

func utsnameToString(field []int8) string {
	b := make([]byte, 0, len(field))
	for _, c := range field {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

func gatherUptime() *SysinfoUptime {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return nil
	}
	secondsF, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil
	}
	d := time.Duration(secondsF) * time.Second
	return &SysinfoUptime{Seconds: int64(secondsF), Human: humanDuration(d)}
}

func humanDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func gatherCPU() *SysinfoCPU {
	cpu := &SysinfoCPU{Threads: runtime.NumCPU()}

	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		coreIDs := map[string]bool{}
		var curPhys string
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			switch key {
			case "model name":
				if cpu.Model == "" {
					cpu.Model = val
				}
			case "physical id":
				curPhys = val
			case "core id":
				coreIDs[curPhys+"/"+val] = true
			}
		}
		if len(coreIDs) > 0 {
			cpu.Cores = len(coreIDs)
		} else {
			cpu.Cores = cpu.Threads
		}
	} else {
		cpu.Cores = cpu.Threads
	}

	if la, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(la))
		if len(fields) >= 3 {
			cpu.LoadAvg1, _ = strconv.ParseFloat(fields[0], 64)
			cpu.LoadAvg5, _ = strconv.ParseFloat(fields[1], 64)
			cpu.LoadAvg15, _ = strconv.ParseFloat(fields[2], 64)
		}
	}

	cpu.UsagePct = sampleCPUUsagePct(100 * time.Millisecond)

	if cpu.Model == "" && cpu.LoadAvg1 == 0 && cpu.UsagePct == 0 {
		return nil
	}
	return cpu
}

// sampleCPUUsagePct takes two /proc/stat samples interval apart and returns
// the percentage of non-idle time between them.
func sampleCPUUsagePct(interval time.Duration) float64 {
	idle1, total1, ok1 := readProcStatTotals()
	if !ok1 {
		return 0
	}
	time.Sleep(interval)
	idle2, total2, ok2 := readProcStatTotals()
	if !ok2 || total2 <= total1 {
		return 0
	}
	idleDelta := float64(idle2 - idle1)
	totalDelta := float64(total2 - total1)
	if totalDelta <= 0 {
		return 0
	}
	return 100 * (1 - idleDelta/totalDelta)
}

func readProcStatTotals() (idle, total int64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var sum int64
		for i, f := range fields {
			v, err := strconv.ParseInt(f, 10, 64)
			if err != nil {
				continue
			}
			sum += v
			if i == 3 { // idle field
				idle = v
			}
		}
		return idle, sum, true
	}
	return 0, 0, false
}

func gatherMemory() *SysinfoMemory {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	fields := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		fields[strings.TrimSpace(parts[0])] = parseKBField(parts[1])
	}
	mem := &SysinfoMemory{
		TotalKB:     fields["MemTotal"],
		AvailableKB: fields["MemAvailable"],
		SwapTotalKB: fields["SwapTotal"],
	}
	swapFree := fields["SwapFree"]
	mem.SwapUsedKB = mem.SwapTotalKB - swapFree
	mem.UsedKB = mem.TotalKB - mem.AvailableKB
	if mem.TotalKB > 0 {
		mem.UsagePct = 100 * float64(mem.UsedKB) / float64(mem.TotalKB)
	}
	if mem.TotalKB == 0 {
		return nil
	}
	return mem
}

func gatherDisk() []SysinfoDisk {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	skipFSTypes := map[string]bool{
		"proc": true, "sysfs": true, "devtmpfs": true, "tmpfs": true,
		"devpts": true, "cgroup": true, "cgroup2": true, "overlay": true,
		"squashfs": true, "debugfs": true, "tracefs": true, "mqueue": true,
		"pstore": true, "bpf": true, "securityfs": true, "autofs": true,
		"hugetlbfs": true, "configfs": true, "fusectl": true, "rpc_pipefs": true,
	}

	var disks []SysinfoDisk
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		device, mount, fsType := fields[0], fields[1], fields[2]
		if skipFSTypes[fsType] || seen[mount] {
			continue
		}
		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount, &stat); err != nil {
			continue
		}
		blockSize := int64(stat.Bsize)
		totalKB := int64(stat.Blocks) * blockSize / 1024
		availKB := int64(stat.Bavail) * blockSize / 1024
		freeKB := int64(stat.Bfree) * blockSize / 1024
		usedKB := totalKB - freeKB
		if totalKB == 0 {
			continue
		}
		seen[mount] = true
		disks = append(disks, SysinfoDisk{
			Mount: mount, Device: device, FSType: fsType,
			TotalKB: totalKB, UsedKB: usedKB, AvailableKB: availKB,
			UsagePct: 100 * float64(usedKB) / float64(totalKB),
		})
	}
	return disks
}

func gatherNetwork() []SysinfoNetworkIface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []SysinfoNetworkIface
	for _, iface := range ifaces {
		state := "down"
		if iface.Flags&net.FlagUp != 0 {
			state = "up"
		}
		entry := SysinfoNetworkIface{Name: iface.Name, State: state, MAC: iface.HardwareAddr.String()}

		addrs, err := iface.Addrs()
		if err == nil {
			for _, a := range addrs {
				ipNet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				if ip4 := ipNet.IP.To4(); ip4 != nil {
					if entry.IPv4 == "" {
						entry.IPv4 = ip4.String()
					}
				} else if entry.IPv6 == "" {
					entry.IPv6 = ipNet.IP.String()
				}
			}
		}
		out = append(out, entry)
	}
	return out
}
