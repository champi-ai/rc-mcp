// This file implements the process_list/process_info/process_signal
// executors by reading /proc directly (Linux). See docs/specs/backend.md
// Section 3.4.
package executor

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// clockTicksPerSec is HZ (sysconf(_SC_CLK_TCK)). 100 is the value on every
// mainstream Linux distro/architecture this agent targets; parsing it out
// of sysconf would require cgo, which the rest of the agent avoids.
const clockTicksPerSec = 100

// ErrProcessNotFound is returned when the given PID has no /proc entry (or
// exited between being listed and being read).
var ErrProcessNotFound = errors.New("executor: process not found")

// ErrSelfSignalRejected is returned by SendProcessSignal when asked to
// signal the agent's own PID -- a hard reject per Section 3.4.3, even if
// the server forwarded it.
var ErrSelfSignalRejected = errors.New("executor: refusing to signal the agent's own process")

// ProcessInfo is the full set of fields FSStat-style callers may want;
// process_list only surfaces a subset (see ProcessListEntry).
type ProcessInfo struct {
	PID       int
	PPID      int
	Name      string
	Cmdline   string
	Exe       string
	Cwd       string
	User      string
	State     string
	Threads   int
	CPUPct    float64
	MemPct    float64
	MemRSSKB  int64
	MemVMSKB  int64
	StartTime time.Time
	FDs       int
	Environ   map[string]string
}

// ListProcesses returns processes matching filter (substring of Name) and
// userFilter (exact match), sorted by sortBy ("pid" default | "cpu" |
// "memory" | "name"), capped at limit (default 100). totalCount is the
// number of processes that matched the filters, before the limit is
// applied.
func ListProcesses(filter, userFilter, sortBy string, limit int) (procs []ProcessInfo, totalCount int, err error) {
	pids, err := listPIDs()
	if err != nil {
		return nil, 0, fmt.Errorf("process_list: %w", err)
	}

	memTotalKB := systemMemTotalKB()
	bootTime := systemBootTime()

	var all []ProcessInfo
	for _, pid := range pids {
		info, err := readProcess(pid, false, bootTime, memTotalKB)
		if err != nil {
			continue // process exited between readdir and stat, or unreadable
		}
		if filter != "" && !strings.Contains(info.Name, filter) {
			continue
		}
		if userFilter != "" && info.User != userFilter {
			continue
		}
		all = append(all, info)
	}

	switch sortBy {
	case "cpu":
		sort.Slice(all, func(i, j int) bool { return all[i].CPUPct > all[j].CPUPct })
	case "memory":
		sort.Slice(all, func(i, j int) bool { return all[i].MemRSSKB > all[j].MemRSSKB })
	case "name":
		sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	default:
		sort.Slice(all, func(i, j int) bool { return all[i].PID < all[j].PID })
	}

	totalCount = len(all)
	if limit <= 0 {
		limit = 100
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, totalCount, nil
}

// GetProcessInfo returns full details for pid, or ErrProcessNotFound.
func GetProcessInfo(pid int) (ProcessInfo, error) {
	info, err := readProcess(pid, true, systemBootTime(), systemMemTotalKB())
	if err != nil {
		return ProcessInfo{}, err
	}
	return info, nil
}

// SendProcessSignal sends signalName (default SIGTERM if empty/unknown) to
// pid, refusing to target the agent's own process.
func SendProcessSignal(pid int, signalName string) (resolvedSignal string, err error) {
	if pid == os.Getpid() {
		return "", ErrSelfSignalRejected
	}
	sig, ok := signalTable[signalName]
	if !ok {
		signalName = "SIGTERM"
		sig = syscall.SIGTERM
	}

	if err := syscall.Kill(pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return signalName, ErrProcessNotFound
		}
		return signalName, fmt.Errorf("process_signal: %w", err)
	}
	return signalName, nil
}

var signalTable = map[string]syscall.Signal{
	"SIGTERM": syscall.SIGTERM,
	"SIGKILL": syscall.SIGKILL,
	"SIGHUP":  syscall.SIGHUP,
	"SIGINT":  syscall.SIGINT,
	"SIGUSR1": syscall.SIGUSR1,
	"SIGUSR2": syscall.SIGUSR2,
	"SIGSTOP": syscall.SIGSTOP,
	"SIGCONT": syscall.SIGCONT,
}

func listPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func readProcess(pid int, full bool, bootTime time.Time, memTotalKB int64) (ProcessInfo, error) {
	statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ProcessInfo{}, ErrProcessNotFound
	}
	s := string(statData)
	lparen := strings.IndexByte(s, '(')
	rparen := strings.LastIndexByte(s, ')')
	if lparen < 0 || rparen < 0 || rparen < lparen || rparen+2 > len(s) {
		return ProcessInfo{}, fmt.Errorf("process: malformed /proc/%d/stat", pid)
	}
	name := s[lparen+1 : rparen]
	fields := strings.Fields(s[rparen+2:])
	// Fields (0-indexed from after "comm)"): 0=state 1=ppid ... 11=utime
	// 12=stime ... 19=starttime. See proc(5).
	if len(fields) < 20 {
		return ProcessInfo{}, fmt.Errorf("process: unexpected /proc/%d/stat field count", pid)
	}
	state := procStateDesc(fields[0])
	ppid, _ := strconv.Atoi(fields[1])
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	starttimeTicks, _ := strconv.ParseInt(fields[19], 10, 64)

	startTime := bootTime.Add(time.Duration(starttimeTicks) * time.Second / clockTicksPerSec)
	cpuPct := 0.0
	if age := time.Since(startTime).Seconds(); age > 0 {
		cpuPct = 100 * (float64(utime+stime) / clockTicksPerSec) / age
	}

	status := parseProcStatus(pid)
	threads, _ := strconv.Atoi(status["Threads"])
	rssKB := parseKBField(status["VmRSS"])
	vmsKB := parseKBField(status["VmSize"])
	uidField := strings.Fields(status["Uid"])
	username := ""
	if len(uidField) > 0 {
		username = lookupUsername(uidField[0])
	}

	memPct := 0.0
	if memTotalKB > 0 {
		memPct = 100 * float64(rssKB) / float64(memTotalKB)
	}

	info := ProcessInfo{
		PID: pid, PPID: ppid, Name: name, User: username, State: state,
		Threads: threads, CPUPct: cpuPct, MemPct: memPct, MemRSSKB: rssKB, MemVMSKB: vmsKB,
		StartTime: startTime,
	}

	cmdlineData, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	info.Cmdline = strings.TrimSpace(strings.ReplaceAll(string(cmdlineData), "\x00", " "))

	if full {
		info.Exe, _ = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		info.Cwd, _ = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
			info.FDs = len(fds)
		}
		if environData, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
			info.Environ = parseEnviron(environData)
		}
	}

	return info, nil
}

func procStateDesc(code string) string {
	// Keep the raw /proc state letter; MCP clients can interpret it (R, S,
	// D, Z, T, etc. per proc(5)) without us maintaining a translation table
	// that could drift from the kernel's.
	return code
}

func parseProcStatus(pid int) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return out
}

func parseKBField(v string) int64 {
	v = strings.TrimSuffix(strings.TrimSpace(v), " kB")
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n
}

func parseEnviron(data []byte) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(string(data), "\x00") {
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func lookupUsername(uid string) string {
	if u, err := user.LookupId(uid); err == nil {
		return u.Username
	}
	return uid
}

func systemMemTotalKB() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			return parseKBField(strings.TrimPrefix(line, "MemTotal:"))
		}
	}
	return 0
}

func systemBootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "btime ") {
			secs, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
			if err == nil {
				return time.Unix(secs, 0).UTC()
			}
		}
	}
	return time.Time{}
}
