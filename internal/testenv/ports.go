package testenv

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const testEnginePortRangeSize = 64

var testPortRangeMu sync.Mutex
var testPortRanges = map[int]struct{}{}

// claimStart reserves a range start for the machine, not just this process:
// `go test ./...` runs one binary per package, so the process-local map cannot
// see a sibling's pick, and the Listen probe below only rejects ports that
// are bound at probe time — never a port a sibling picked a moment ago and
// binds seconds later. The O_EXCL marker file in the temp dir closes that
// window; a marker left by a dead process is reclaimed so killed runs cannot
// starve the pool.
func claimStart(start int) bool {
	name := filepath.Join(os.TempDir(), fmt.Sprintf("gpuworks-testport-%d", start))
	// Write the pid into a staging file first and claim with link(2): link
	// fails if the marker exists, and the marker never appears with a
	// half-written pid a concurrent reader could mistake for a dead owner
	// (a torn read like "73" of "73825" would let a sibling steal a range
	// the live owner is about to return).
	staging := name + fmt.Sprintf(".%d.tmp", os.Getpid())
	if err := os.WriteFile(staging, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return false
	}
	if err := os.Link(staging, name); err == nil {
		_ = os.Remove(staging)
		return true
	}
	_ = os.Remove(staging)
	owner, err := os.ReadFile(name)
	if err != nil {
		return false
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(owner)))
	if convErr != nil || pid == os.Getpid() || processAlive(pid) {
		return false
	}
	return os.Remove(name) == nil && claimStart(start)
}

// releasePortRange hands a range chosen by chooseTestPortRange back to the
// pool, for this process and for the machine.
// shuffledSlots is 0..n-1 in a random order (Fisher-Yates over crypto/rand, so
// independent test processes do not share a sequence).
func shuffledSlots(n int) ([]int, error) {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j, err := crand.Int(crand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, err
		}
		order[i], order[j.Int64()] = order[j.Int64()], order[i]
	}
	return order, nil
}

func releasePortRange(start int) {
	testPortRangeMu.Lock()
	delete(testPortRanges, start)
	testPortRangeMu.Unlock()
	releaseStart(start)
}

func releaseStart(start int) {
	_ = os.Remove(filepath.Join(os.TempDir(), fmt.Sprintf("gpuworks-testport-%d", start)))
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	// EPERM means the process exists but belongs to another user; only
	// ESRCH (wrapped as os.ErrProcessDone or the raw errno) marks it dead.
	return err == nil || !errors.Is(err, os.ErrProcessDone) && err != syscall.ESRCH
}

// chooseTestPortRange reserves a currently-free range for a harness. The
// random candidate plus the machine-wide claim keeps independent go test
// processes from converging on the same range; probing every port avoids
// selecting a range already occupied by a non-test listener. A zero result
// retains the production default as a final fallback.
//
// Candidates sit on a rangeSize grid: the marker file claims only the range
// start, so two harnesses whose starts are less than a range apart could
// claim different markers yet still overlap and collide later, when their
// engines actually bind. Grid-aligned starts make disjoint ranges follow
// from distinct starts by construction.
func chooseTestPortRange() (int, int) {
	const minStart = 30000
	const lastStart = 32767 - testEnginePortRangeSize + 1
	const gridSteps = (lastStart - minStart) / testEnginePortRangeSize
	testPortRangeMu.Lock()
	defer testPortRangeMu.Unlock()
	// Every slot is tried, in random order: a few random picks miss the one
	// or two free slots of a crowded pool, and the fallback to the production
	// default range is what sibling test processes then collide on.
	order, err := shuffledSlots(gridSteps + 1)
	if err != nil {
		return 0, 0
	}
	for _, slot := range order {
		start := minStart + slot*testEnginePortRangeSize
		if _, taken := testPortRanges[start]; taken {
			continue
		}
		if !claimStart(start) {
			continue
		}
		listeners := make([]net.Listener, 0, testEnginePortRangeSize)
		ok := true
		for p := start; p < start+testEnginePortRangeSize; p++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				ok = false
				break
			}
			listeners = append(listeners, ln)
		}
		for _, ln := range listeners {
			_ = ln.Close()
		}
		if ok {
			testPortRanges[start] = struct{}{}
			return start, start + testEnginePortRangeSize - 1
		}
		// The range is unusable for real listeners; give the marker back so
		// the failure does not shadow the start for sibling processes.
		releaseStart(start)
	}
	return 0, 0
}
