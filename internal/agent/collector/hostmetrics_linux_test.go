//go:build linux

package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeProcFile(t *testing.T, procRoot, relPath, content string) {
	t.Helper()
	full := filepath.Join(procRoot, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadCPUTicksMissingFileReturnsNotOK(t *testing.T) {
	if _, ok := readCPUTicks(t.TempDir()); ok {
		t.Fatal("expected not-ok for a proc root with no stat file")
	}
}

func TestHostSamplerCPUBusyPercentAcrossTwoTicks(t *testing.T) {
	procRoot := t.TempDir()
	writeProcFile(t, procRoot, "stat", "cpu  100 0 100 700 50 0 0 0 0 0\nintr 0\n")
	h := &hostSampler{procRoot: procRoot}
	first := h.sample("node-1", "", 1)
	if len(first) != 0 {
		t.Fatalf("first tick must not emit a rate sample (no prior reading): %+v", first)
	}
	// user +200, system +100, idle +100 (idleAll delta 100), total delta 400.
	writeProcFile(t, procRoot, "stat", "cpu  300 0 200 800 50 0 0 0 0 0\nintr 0\n")
	second := h.sample("node-1", "", 2)
	if len(second) != 1 {
		t.Fatalf("second tick samples = %+v, want exactly the CPU busy percent", second)
	}
	s := second[0]
	if s.Metric != "gpuworks_node_cpu_busy_percent" || s.Labels["node"] != "node-1" || s.TimeMS != 2 {
		t.Fatalf("sample = %+v", s)
	}
	if want := 75.0; s.Value < want-0.01 || s.Value > want+0.01 {
		t.Fatalf("busy percent = %v, want %v", s.Value, want)
	}
}

func TestHostSamplerCPUCounterRollbackIsTreatedAsNoMovement(t *testing.T) {
	procRoot := t.TempDir()
	writeProcFile(t, procRoot, "stat", "cpu  300 0 200 800 50 0 0 0 0 0\nintr 0\n")
	h := &hostSampler{procRoot: procRoot}
	h.sample("node-1", "", 1)
	// A smaller total than before (host rebooted) must not produce a division
	// by a bogus negative delta or a wild percentage.
	writeProcFile(t, procRoot, "stat", "cpu  10 0 5 50 0 0 0 0 0 0\nintr 0\n")
	got := h.sample("node-1", "", 2)
	for _, s := range got {
		if s.Metric == "gpuworks_node_cpu_busy_percent" {
			t.Fatalf("counter rollback must not emit a bogus rate: %+v", s)
		}
	}
}

func TestResolveDeviceNumberNonexistentPathReturnsNotOK(t *testing.T) {
	if _, _, ok := resolveDeviceNumber(filepath.Join(t.TempDir(), "does-not-exist")); ok {
		t.Fatal("expected not-ok for a path that does not exist")
	}
}

func writeDiskStats(t *testing.T, procRoot string, major, minor uint32, name string, sectorsRead, sectorsWritten uint64) {
	t.Helper()
	// Faithful 14-field /proc/diskstats row shape (major minor name then 11
	// counters); only sectors-read (field 6) and sectors-written (field 10)
	// matter here, the rest are filled with plausible non-zero placeholders.
	line := formatDiskStatsLine(major, minor, name, sectorsRead, sectorsWritten)
	writeProcFile(t, procRoot, "diskstats", line)
}

func formatDiskStatsLine(major, minor uint32, name string, sectorsRead, sectorsWritten uint64) string {
	return strconv.FormatUint(uint64(major), 10) + " " + strconv.FormatUint(uint64(minor), 10) + " " + name +
		" 100 5 " + strconv.FormatUint(sectorsRead, 10) + " 20 200 10 " + strconv.FormatUint(sectorsWritten, 10) + " 40 0 60 100\n"
}

func TestReadDiskCountersNoMatchingDeviceReturnsNotOK(t *testing.T) {
	procRoot := t.TempDir()
	writeDiskStats(t, procRoot, 8, 0, "sda", 1000, 2000)
	if _, ok := readDiskCounters(procRoot, 253, 1); ok {
		t.Fatal("expected not-ok when no diskstats row matches the major/minor")
	}
}

func TestHostSamplerDiskRatesForResolvedDevice(t *testing.T) {
	modelsRoot := t.TempDir()
	major, minor, ok := resolveDeviceNumber(modelsRoot)
	if !ok {
		t.Skip("host filesystem did not resolve a block device for a temp directory")
	}
	procRoot := t.TempDir()
	writeDiskStats(t, procRoot, major, minor, "sda", 1000, 2000)
	// A fake, injected clock makes the elapsed-time denominator exact instead
	// of depending on real wall-clock scheduling jitter between the two
	// sample() calls below - under system load that jitter is easily enough
	// to blow past a percentage tolerance on a short 2s window.
	fakeNow := time.Now()
	h := &hostSampler{procRoot: procRoot, now: func() time.Time { return fakeNow }}
	if got := h.sample("node-1", modelsRoot, 1); len(got) != 0 {
		t.Fatalf("first tick must not emit disk rates: %+v", got)
	}
	fakeNow = fakeNow.Add(2 * time.Second)
	writeDiskStats(t, procRoot, major, minor, "sda", 1000+1000, 2000+1000) // +512000B read, +512000B written
	got := h.sample("node-1", modelsRoot, 2)
	byName := map[string]float64{}
	for _, s := range got {
		if s.Labels["device"] != "sda" || s.Labels["node"] != "node-1" {
			t.Fatalf("labels = %+v", s.Labels)
		}
		byName[s.Metric] = s.Value
	}
	const wantBytesPerSec = 512000.0 / 2.0
	for _, metric := range []string{"gpuworks_node_disk_read_bytes_per_sec", "gpuworks_node_disk_write_bytes_per_sec"} {
		v, ok := byName[metric]
		if !ok {
			t.Fatalf("missing %s in %+v", metric, byName)
		}
		if v != wantBytesPerSec {
			t.Fatalf("%s = %v, want exactly %v (clock is fake and exact)", metric, v, wantBytesPerSec)
		}
	}
}

func TestHostSamplerDiskSeriesSkippedWhenModelsRootEmpty(t *testing.T) {
	procRoot := t.TempDir()
	writeDiskStats(t, procRoot, 8, 0, "sda", 1000, 2000)
	h := &hostSampler{procRoot: procRoot}
	h.sample("node-1", "", 1)
	got := h.sample("node-1", "", 2)
	for _, s := range got {
		if strings.HasPrefix(s.Metric, "gpuworks_node_disk_") {
			t.Fatalf("empty modelsRoot must skip disk series: %+v", got)
		}
	}
}

func TestReadNetCountersSumsNonLoopbackInterfaces(t *testing.T) {
	procRoot := t.TempDir()
	writeProcFile(t, procRoot, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  500       10    0    0    0     0          0         0     500      10    0    0    0     0       0          0
  eth0: 1000       20    0    0    0     0          0         0    2000      30    0    0    0     0       0          0
  eth1:  200        5    0    0    0     0          0         0     300       6    0    0    0     0       0          0
`)
	cur, ok := readNetCounters(procRoot)
	if !ok {
		t.Fatal("expected ok with at least one non-loopback interface")
	}
	if cur.rxBytes != 1200 || cur.txBytes != 2300 {
		t.Fatalf("counters = %+v, want rx=1200 tx=2300 (loopback excluded)", cur)
	}
}

// A real container host's /proc/net/dev carries the physical interface plus
// a handful of virtual ones that mirror or loop back the same traffic
// (docker0, a per-network br-<id>, one veth<id>@if<n> per container, plus
// CNI/flannel/tap/tun variants other runtimes add). Counting those inflates
// the reported host network rate 2-3x; only the physical interface's bytes
// must be summed.
func TestReadNetCountersExcludesContainerHostVirtualInterfaces(t *testing.T) {
	procRoot := t.TempDir()
	writeProcFile(t, procRoot, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:     500       10    0    0    0     0          0         0      500      10    0    0    0     0       0          0
   eth0: 1000000     2000    0    0    0     0          0         0  2000000     3000    0    0    0     0       0          0
docker0:  400000      800    0    0    0     0          0         0   400000      800    0    0    0     0       0          0
br-1234567890ab:  90000      70    0    0    0     0          0         0    90000      70    0    0    0     0       0          0
veth1a2b3c4@if5:  60000      50    0    0    0     0          0         0    60000      50    0    0    0     0       0          0
   cni0:  30000       20    0    0    0     0          0         0    30000      20    0    0    0     0       0          0
flannel.1:  20000      15    0    0    0     0          0         0    20000      15    0    0    0     0       0          0
   tap0:  10000        8    0    0    0     0          0         0    10000       8    0    0    0     0       0          0
  tun0:    5000        4    0    0    0     0          0         0     5000       4    0    0    0     0       0          0
 virbr0:    4000        3    0    0    0     0          0         0     4000       3    0    0    0     0       0          0
`)
	cur, ok := readNetCounters(procRoot)
	if !ok {
		t.Fatal("expected ok with the physical interface present")
	}
	if cur.rxBytes != 1000000 || cur.txBytes != 2000000 {
		t.Fatalf("counters = %+v, want only eth0's rx=1000000 tx=2000000 (every virtual interface excluded)", cur)
	}
}

func TestReadNetCountersOnlyLoopbackReturnsNotOK(t *testing.T) {
	procRoot := t.TempDir()
	writeProcFile(t, procRoot, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  500       10    0    0    0     0          0         0     500      10    0    0    0     0       0          0
`)
	if _, ok := readNetCounters(procRoot); ok {
		t.Fatal("expected not-ok when only loopback is present")
	}
}

func TestHostSamplerNetRatesAcrossTwoTicks(t *testing.T) {
	procRoot := t.TempDir()
	netDev := func(rx, tx int) string {
		return "Inter-|   Receive                                                |  Transmit\n" +
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
			"  eth0: " + strconv.Itoa(rx) + " 20    0    0    0     0          0         0    " + strconv.Itoa(tx) + "      30    0    0    0     0       0          0\n"
	}
	writeProcFile(t, procRoot, "net/dev", netDev(1000, 2000))
	// A fake, injected clock makes the elapsed-time denominator exact - see
	// TestHostSamplerDiskRatesForResolvedDevice's comment for why real
	// wall-clock jitter is a real flake risk here.
	fakeNow := time.Now()
	h := &hostSampler{procRoot: procRoot, now: func() time.Time { return fakeNow }}
	if got := h.sample("node-1", "", 1); len(got) != 0 {
		t.Fatalf("first tick must not emit net rates: %+v", got)
	}
	fakeNow = fakeNow.Add(2 * time.Second)
	writeProcFile(t, procRoot, "net/dev", netDev(2000, 3000)) // +1000B rx, +1000B tx over 2s
	got := h.sample("node-1", "", 2)
	byName := map[string]float64{}
	for _, s := range got {
		byName[s.Metric] = s.Value
	}
	const want = 500.0 // 1000B / 2s
	for _, metric := range []string{"gpuworks_node_net_rx_bytes_per_sec", "gpuworks_node_net_tx_bytes_per_sec"} {
		v, ok := byName[metric]
		if !ok {
			t.Fatalf("missing %s in %+v", metric, byName)
		}
		if v != want {
			t.Fatalf("%s = %v, want exactly %v (clock is fake and exact)", metric, v, want)
		}
	}
}
