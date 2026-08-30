package executor

import (
	"os"
	"testing"
)

func TestListProcesses_FindsSelf(t *testing.T) {
	procs, total, err := ListProcesses("", "", "pid", 100000)
	if err != nil {
		t.Fatalf("ListProcesses: %v", err)
	}
	if total == 0 {
		t.Fatal("expected at least one process (this test process)")
	}

	found := false
	for _, p := range procs {
		if p.PID == os.Getpid() {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the current test process to appear in the list")
	}
}

func TestListProcesses_LimitAndSort(t *testing.T) {
	procs, total, err := ListProcesses("", "", "pid", 2)
	if err != nil {
		t.Fatalf("ListProcesses: %v", err)
	}
	if len(procs) > 2 {
		t.Fatalf("len(procs) = %d, want <= 2", len(procs))
	}
	if total < len(procs) {
		t.Fatalf("total = %d should be >= returned count %d", total, len(procs))
	}
	for i := 1; i < len(procs); i++ {
		if procs[i].PID < procs[i-1].PID {
			t.Fatalf("expected pid-ascending order, got %d before %d", procs[i-1].PID, procs[i].PID)
		}
	}
}

func TestGetProcessInfo_Self(t *testing.T) {
	info, err := GetProcessInfo(os.Getpid())
	if err != nil {
		t.Fatalf("GetProcessInfo(self): %v", err)
	}
	if info.PID != os.Getpid() {
		t.Fatalf("PID = %d, want %d", info.PID, os.Getpid())
	}
	if info.State == "" {
		t.Fatal("expected non-empty state")
	}
}

func TestGetProcessInfo_UnknownPID(t *testing.T) {
	// PID 1<<30 is exceedingly unlikely to exist.
	if _, err := GetProcessInfo(1 << 30); err != ErrProcessNotFound {
		t.Fatalf("err = %v, want ErrProcessNotFound", err)
	}
}

func TestSendProcessSignal_RejectsSelf(t *testing.T) {
	if _, err := SendProcessSignal(os.Getpid(), "SIGTERM"); err != ErrSelfSignalRejected {
		t.Fatalf("err = %v, want ErrSelfSignalRejected", err)
	}
}

func TestSendProcessSignal_UnknownPID(t *testing.T) {
	_, err := SendProcessSignal(1<<30, "SIGCONT")
	if err != ErrProcessNotFound {
		t.Fatalf("err = %v, want ErrProcessNotFound", err)
	}
}

func TestSendProcessSignal_DefaultsToSIGTERM(t *testing.T) {
	resolved, err := SendProcessSignal(1<<30, "")
	if err != ErrProcessNotFound {
		t.Fatalf("err = %v, want ErrProcessNotFound", err)
	}
	if resolved != "SIGTERM" {
		t.Fatalf("resolved signal = %q, want SIGTERM", resolved)
	}
}
