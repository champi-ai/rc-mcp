package executor

import (
	"testing"
	"time"
)

func TestGatherSysinfo_AllSections(t *testing.T) {
	result := GatherSysinfo(nil)
	if result.Hostname == "" {
		t.Fatal("expected non-empty hostname")
	}
	if result.CPU == nil {
		t.Fatal("expected cpu section")
	}
	if result.Memory == nil {
		t.Fatal("expected memory section")
	}
	if result.Uptime == nil {
		t.Fatal("expected uptime section")
	}
}

func TestGatherSysinfo_OnlyCPU(t *testing.T) {
	result := GatherSysinfo([]string{"cpu"})
	if result.CPU == nil {
		t.Fatal("expected cpu section to be populated")
	}
	if result.Memory != nil {
		t.Fatal("expected memory section to be nil when only cpu requested")
	}
	if result.Disk != nil {
		t.Fatal("expected disk section to be nil when only cpu requested")
	}
	if result.Hostname != "" {
		t.Fatal("expected hostname to be empty when only cpu requested")
	}
}

func TestGatherSysinfo_AllKeyword(t *testing.T) {
	result := GatherSysinfo([]string{"all"})
	if result.CPU == nil || result.Memory == nil {
		t.Fatal("expected all sections to be populated with sections=[\"all\"]")
	}
}

func TestGatherCPU_HasThreads(t *testing.T) {
	cpu := gatherCPU()
	if cpu == nil {
		t.Fatal("expected non-nil cpu section on a real host")
	}
	if cpu.Threads <= 0 {
		t.Fatalf("Threads = %d, want > 0", cpu.Threads)
	}
	if cpu.Cores <= 0 {
		t.Fatalf("Cores = %d, want > 0", cpu.Cores)
	}
}

func TestGatherMemory_HasTotal(t *testing.T) {
	mem := gatherMemory()
	if mem == nil {
		t.Fatal("expected non-nil memory section on a real host")
	}
	if mem.TotalKB <= 0 {
		t.Fatalf("TotalKB = %d, want > 0", mem.TotalKB)
	}
}

func TestGatherDisk_ReturnsAtLeastRoot(t *testing.T) {
	disks := gatherDisk()
	if len(disks) == 0 {
		t.Fatal("expected at least one disk entry (root filesystem)")
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{30, "0m"},
		{3661, "1h 1m"},
	}
	for _, c := range cases {
		got := humanDuration(time.Duration(c.secs) * time.Second)
		if got != c.want {
			t.Errorf("humanDuration(%ds) = %q, want %q", c.secs, got, c.want)
		}
	}
}
