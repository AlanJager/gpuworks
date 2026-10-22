//go:build linux

package fleet

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// requireFileDescriptors raises the soft descriptor limit toward the hard one
// so the fleet's sockets fit. A fleet that silently ran under the limit would
// report connection failures as product defects, so a limit that cannot be
// raised stops the case here - as a failure when the suite is declared strict,
// and as an explicit skip otherwise.
func requireFileDescriptors(t *testing.T, want int) {
	t.Helper()
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("read the descriptor limit: %v", err)
	}
	if limit.Cur >= uint64(want) {
		return
	}
	raised := limit
	raised.Cur = limit.Max
	if uint64(want) < limit.Max {
		raised.Cur = uint64(want)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &raised); err == nil && raised.Cur >= uint64(want) {
		t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit) })
		return
	}
	message := "this fleet size needs " + strconv.Itoa(want) + " file descriptors; the hard limit is " + strconv.FormatUint(limit.Max, 10)
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatal(message)
	}
	t.Skip(message + " (set GPUWORKS_FLEET_NODES lower, or raise ulimit -n)")
}

// processRSSMiB reads the resident set size of this process, which is the
// figure that says whether a fleet of this size fits on the machine.
func processRSSMiB() float64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0
	}
	return pages * float64(os.Getpagesize()) / (1 << 20)
}
