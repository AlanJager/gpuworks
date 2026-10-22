//go:build multiproc && linux

package l2

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Reading the two processes separately is the point of the process-separated
// shape: a single-process run reports one number for the server and the fleet
// together, which cannot answer what the control plane costs.

// readProcessRSSMiB is the resident set size of one process.
func readProcessRSSMiB(pid int) float64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
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

// readProcessCPU is the user+system time one process has burned, in seconds.
// Sampled before and after the churn, the difference is what that process spent
// on the workload.
func readProcessCPU(pid int) float64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// The command field can contain spaces and is parenthesised, so fields are
	// counted from after the closing parenthesis: utime and stime are the
	// 14th and 15th of the whole line, i.e. the 12th and 13th after it.
	closing := strings.LastIndex(string(data), ")")
	if closing < 0 {
		return 0
	}
	fields := strings.Fields(string(data)[closing+1:])
	if len(fields) < 13 {
		return 0
	}
	utime, err := strconv.ParseFloat(fields[11], 64)
	if err != nil {
		return 0
	}
	stime, err := strconv.ParseFloat(fields[12], 64)
	if err != nil {
		return 0
	}
	ticks := float64(100) // CLK_TCK is 100 on every Linux port Go supports.
	return (utime + stime) / ticks
}

// requireFleetFileDescriptors raises the soft descriptor limit toward the hard
// one so both processes' sockets fit; children inherit it. A fleet that
// silently ran under the limit would report connection failures as product
// defects, so a limit that cannot be raised stops the case here.
func requireFleetFileDescriptors(t *testing.T, want int) {
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

// readListenOverflows is the host's TcpExt ListenOverflows counter: SYNs
// dropped because a listen backlog was full. Sampled around a load window it
// says whether latency tails came from the kernel (SYN retransmits wait 1s)
// rather than from the product.
func readListenOverflows() int64 {
	data, err := os.ReadFile("/proc/net/netstat")
	if err != nil {
		return -1
	}
	lines := strings.Split(string(data), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "TcpExt:") || !strings.HasPrefix(lines[i+1], "TcpExt:") {
			continue
		}
		names, values := strings.Fields(lines[i]), strings.Fields(lines[i+1])
		for j := range names {
			if names[j] == "ListenOverflows" && j < len(values) {
				n, err := strconv.ParseInt(values[j], 10, 64)
				if err != nil {
					return -1
				}
				return n
			}
		}
	}
	return -1
}
