package testenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPortRangeClaimIsExclusivePerMachineAndReclaimsDeadOwners(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // os.TempDir reads TMPDIR on each call
	const start = 31000
	marker := filepath.Join(os.TempDir(), fmt.Sprintf("gpuworks-testport-%d", start))
	if !claimStart(start) {
		t.Fatal("first claim rejected")
	}
	if claimStart(start) {
		t.Fatal("same process claimed one range twice")
	}
	if err := os.WriteFile(marker, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if claimStart(start) {
		t.Fatal("stole a range from a live sibling process")
	}
	if err := os.WriteFile(marker, []byte("4194305"), 0o600); err != nil { // > pid_max, cannot be alive
		t.Fatal(err)
	}
	if !claimStart(start) {
		t.Fatal("dead owner's marker starved the pool")
	}
	if got, _ := os.ReadFile(marker); string(got) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("marker not re-stamped: %q", got)
	}
}

// TestPortRangeClaimIsMachineWideAcrossProcesses proves the property the CI
// failure was about: a range start claimed by one test process is invisible
// to the process-local map of a sibling package binary, so exclusivity has to
// come from the machine-wide marker. The parent claims a start and re-runs
// the test binary as a child that must fail to claim the same start; the
// child exits non-zero when it steals the range.
func TestPortRangeClaimIsMachineWideAcrossProcesses(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PORT_CLAIM_CHILD") == "1" {
		start, _ := strconv.Atoi(os.Getenv("GPUWORKS_TEST_PORT_CLAIM_START"))
		if claimStart(start) {
			os.Exit(1) // stole a machine-claimed range
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	const start = 31100
	if !claimStart(start) {
		t.Fatal("parent claim rejected")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPortRangeClaimIsMachineWideAcrossProcesses$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"GPUWORKS_TEST_PORT_CLAIM_CHILD=1",
		"GPUWORKS_TEST_PORT_CLAIM_START="+strconv.Itoa(start),
		"TMPDIR="+dir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child stole a machine-claimed range: %v\n%s", err, out)
	}
}

// The grid alignment in chooseTestPortRange is what keeps concurrent test
// packages' ranges disjoint: the marker file claims only the start, so every
// pick must sit on the rangeSize grid inside the engine-port window, and two
// picks in one process must never share a start. Off-grid starts from two
// harnesses could claim different markers yet still overlap when their
// engines bind (the 32036 collision, pipeline 181396).
func TestChooseTestPortRangeGridAligned(t *testing.T) {
	const minStart = 30000
	const maxPort = 32767
	seen := map[int]bool{}
	defer func() {
		for start := range seen {
			releaseStart(start)
		}
	}()
	for i := 0; i < 4; i++ {
		start, end := chooseTestPortRange()
		if start < minStart || end > maxPort {
			t.Fatalf("range [%d,%d] escapes the engine-port window [%d,%d]", start, end, minStart, maxPort)
		}
		if (start-minStart)%testEnginePortRangeSize != 0 {
			t.Fatalf("start %d is off the %d-port grid", start, testEnginePortRangeSize)
		}
		if end-start+1 != testEnginePortRangeSize {
			t.Fatalf("range [%d,%d] is not %d wide", start, end, testEnginePortRangeSize)
		}
		if seen[start] {
			t.Fatalf("start %d claimed twice in one process", start)
		}
		seen[start] = true
	}
}

// Integration packages create harnesses in successive tests. Each completed
// test must return its actual engine-port claim: both the machine marker and
// the process-local entry. Probe the full real port pool instead of reserving
// most of it and assuming a fixed subset is free of unrelated listeners.
func TestHarnessReturnsItsEnginePortRangeWhenItsTestEnds(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // isolate marker ownership; real OS ports remain shared
	for i := 0; i < 6; i++ {
		var start int
		t.Run(fmt.Sprintf("harness-%d", i), func(t *testing.T) {
			env := New(t)
			start = env.portStart
			if start == 0 {
				t.Fatalf("harness %d found no available engine-port range", i)
			}
		})
		marker := filepath.Join(os.TempDir(), fmt.Sprintf("gpuworks-testport-%d", start))
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("range %d marker was not removed after harness %d ended: %v", start, i, err)
		}
		testPortRangeMu.Lock()
		_, held := testPortRanges[start]
		testPortRangeMu.Unlock()
		if held {
			t.Fatalf("range %d is still held by this process after harness %d ended", start, i)
		}
	}
}
