package udocker

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// RootPID is the recorded leader of the workload's process group: the pid
// file Run writes. A workload with no record, or a record that is not a
// pid, has none.
func TestRootPIDIsTheRecordedGroupLeader(t *testing.T) {
	d := New(t.TempDir())
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if pid, err := d.RootPID(context.Background(), "never-started"); err == nil || pid != 0 {
		t.Fatalf("a workload with no record: RootPID = %d, err=%v", pid, err)
	}
	for name, record := range map[string]string{"empty": "", "garbage": "not-a-pid\n", "zero": "0\n"} {
		if err := os.WriteFile(d.procs().PIDPath(name), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		if pid, err := d.RootPID(context.Background(), name); err == nil || pid != 0 {
			t.Errorf("a %s record: RootPID = %d, err=%v", name, pid, err)
		}
	}
	// A workload started the way Run starts one.
	cmd := startTracked(t, d, "demo", "sleep 30")
	// The reaper records the exit in the state directory: it must have
	// finished before the test's directory is removed.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		waitExited(t, d, "demo")
	})
	pid, err := d.RootPID(context.Background(), "demo")
	if err != nil || pid != cmd.Process.Pid {
		t.Fatalf("RootPID = %d, err=%v; want the started process %d", pid, err, cmd.Process.Pid)
	}
	raw, err := os.ReadFile(d.procs().PIDPath("demo"))
	if err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(pid) {
		t.Fatalf("pid record = %q, err=%v; want the process RootPID answers (%d)", raw, err, pid)
	}
}
