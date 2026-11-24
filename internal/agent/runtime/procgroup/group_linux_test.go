//go:build linux

package procgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func startGroupWithZombie(t *testing.T, tr *Tracker) (pgid, zombiePID int) {
	t.Helper()
	leader := start(t, tr, "serving", "exec sleep 30")
	// Keep this exited child in the group by deferring Wait.
	worker := exec.Command("/bin/sh", "-c", "exit 0")
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Wait() })
	stat := "/proc/" + strconv.Itoa(worker.Process.Pid) + "/stat"
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(stat)
		if err != nil {
			t.Fatal(err)
		}
		if end := bytes.LastIndexByte(raw, ')'); end >= 0 && bytes.HasPrefix(raw[end+1:], []byte(" Z ")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not become a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}

	return leader.Process.Pid, worker.Process.Pid
}

// A zombie this process cannot wait for - its parent is another process that
// is still alive, or an init outside this agent - must not keep a stop
// pending: it holds nothing but a process-table slot, and no signal can move
// it. The reaper is given no part in this one, so what remains is exactly
// what a stop may not wait for.
func TestStopReturnsWhileAZombieItCannotReapRemains(t *testing.T) {
	if !procShowsAllPIDs() {
		t.Skip("unrestricted procfs required")
	}
	tr := &Tracker{Dir: t.TempDir(), KillWait: 100 * time.Millisecond}
	_, zombiePID := startGroupWithZombie(t, tr)
	probe := groupProbe{stillWorking: groupStillWorking, reap: func(int) int { return 0 }}
	if err := tr.stop(context.Background(), "serving", 50*time.Millisecond, probe); err != nil {
		t.Fatalf("Stop with only an unreapable worker left: %v", err)
	}
	if err := syscall.Kill(zombiePID, 0); err != nil {
		t.Fatalf("a stop that may not wait for the zombie reaped it anyway: %v", err)
	}
}

// A worker that exits between a drain's reap and its look at the group still
// has to be reaped before the stop returns: once its leader is gone it has
// been handed to this process, and nothing else will ever wait for it. The
// probe holds the reaper back until the drain has judged the group drained,
// which is the order a loaded machine can produce on its own.
func TestStopReapsAWorkerThatExitedAfterTheLastReap(t *testing.T) {
	if !procShowsAllPIDs() {
		t.Skip("unrestricted procfs required")
	}
	tr := &Tracker{Dir: t.TempDir(), KillWait: 100 * time.Millisecond}
	_, zombiePID := startGroupWithZombie(t, tr)
	judged := false
	probe := groupProbe{
		stillWorking: func(pgid int) bool {
			working := groupStillWorking(pgid)
			judged = judged || !working
			return working
		},
		reap: func(pgid int) int {
			if !judged {
				return 0
			}
			return reapOrphans(pgid)
		},
	}
	if err := tr.stop(context.Background(), "serving", 50*time.Millisecond, probe); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(zombiePID, 0); err == nil {
		t.Fatalf("the stop returned with its adopted worker %d still <defunct>", zombiePID)
	}
}

func TestGroupLivenessAfterKill(t *testing.T) {
	tests := []struct {
		name    string
		state   byte
		threads int
		want    bool
	}{
		{"zombie", 'Z', 1, false},
		{"dead", 'X', 1, false},
		{"running", 'R', 1, true},
		{"uninterruptible", 'D', 1, true},
		{"zombie leader with another thread", 'Z', 2, true},
		{"incomplete thread count", 'Z', 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proc := t.TempDir()
			const pgid = 100
			writeGroupStat(t, proc, 101, pgid, tc.threads, tc.state)
			writeGroupStat(t, proc, 102, pgid, 1, 'Z')
			writeGroupStat(t, proc, 201, 200, 1, 'R')
			if got := groupHasLiveMembers(proc, pgid); got != tc.want {
				t.Fatalf("group has live members = %t, want %t", got, tc.want)
			}
		})
	}

	t.Run("no matching PID visible", func(t *testing.T) {
		proc := t.TempDir()
		writeGroupStat(t, proc, 102, 100, 1, 'Z')
		if !groupHasLiveMembers(proc, 200) {
			t.Fatal("an unseen group was considered drained")
		}
	})
	t.Run("unreadable member status", func(t *testing.T) {
		proc := t.TempDir()
		writeGroupStat(t, proc, 101, 100, 1, 'Z')
		// An open returns EISDIR regardless of the test user's privileges.
		if err := os.MkdirAll(filepath.Join(proc, "102", "stat"), 0o700); err != nil {
			t.Fatal(err)
		}
		if !groupHasLiveMembers(proc, 100) {
			t.Fatal("a group with unreadable process data was considered drained")
		}
	})
}

func TestStopKeepsAGroupWithUninterruptibleWorkPending(t *testing.T) {
	tr := &Tracker{Dir: t.TempDir(), KillWait: 100 * time.Millisecond}
	pgid, workerPID := startGroupWithZombie(t, tr)
	proc := t.TempDir()
	// The zombie keeps signal 0 succeeding while the snapshot models a
	// worker whose kernel work keeps it in D after SIGKILL.
	writeGroupStat(t, proc, workerPID, pgid, 1, 'D')
	probe := groupProbe{
		stillWorking: func(pgid int) bool { return groupHasLiveMembers(proc, pgid) },
		// The snapshot is the test's; nothing in it may be waited for.
		reap: func(int) int { return 0 },
	}
	err := tr.stop(context.Background(), "serving", 50*time.Millisecond, probe)
	if err == nil || !strings.Contains(err.Error(), "after SIGKILL") || !errors.Is(err, ErrNotDrained) {
		t.Fatalf("Stop with an uninterruptible member = %v, want a pending stop reported as ErrNotDrained", err)
	}
	if _, ok := tr.PID("serving"); !ok {
		t.Fatal("pending stop forgot the group")
	}
	writeGroupStat(t, proc, workerPID, pgid, 1, 'Z')
	if err := tr.stop(context.Background(), "serving", 50*time.Millisecond, probe); err != nil {
		t.Fatalf("retry after the member exited = %v", err)
	}
}

func writeGroupStat(t *testing.T, proc string, pid, pgid, threads int, state byte) {
	t.Helper()
	dir := filepath.Join(proc, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("%d (worker ) task) %c 1 %d %d 0 -1 4194304 74 0 0 0 0 0 0 0 20 0 %d 0 100 1000 10\n", pid, state, pgid, pgid, threads)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}
