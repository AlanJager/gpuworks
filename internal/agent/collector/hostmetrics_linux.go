//go:build linux

// Host CPU/disk/network rate sampling for the collector's 10s tick. Pure
// /proc file reads — no commands, matching the "agent 采集纪律" row in the
// monitor plan (only --query-gpu, dmon and per-instance HTTP scrapes may run
// a process per tick).
package collector

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// hostSampler turns cumulative /proc counters into the rates the monitor
// plan wants. A rate needs two readings, so the tick right after a Collector
// is created (or right after a previously-unreadable source starts reading)
// emits none of the three rate series; the memory gauges in hostSamples are
// unaffected since they need no history.
type hostSampler struct {
	// procRoot defaults to "/proc"; tests point it at a fixture directory.
	procRoot string
	// sysRoot is sysfs ("/sys"); empty skips the per-NIC and per-RDMA-port
	// series (linkmetrics_linux.go).
	sysRoot string

	// now is the clock the disk/net rate calculations read; nil means
	// time.Now. Tests inject a fake clock so the elapsed-time denominator is
	// exact instead of whatever real wall-clock jitter happened to elapse
	// between the two sample() calls under system load.
	now func() time.Time

	mu sync.Mutex

	haveCPU bool
	prevCPU cpuTicks

	haveDisk   bool
	prevDisk   diskCounters
	prevDiskAt time.Time

	haveNet   bool
	prevNet   netCounters
	prevNetAt time.Time

	links linkSampler
}

func newHostSampler() *hostSampler { return &hostSampler{procRoot: "/proc", sysRoot: "/sys"} }

// clockNow returns h's clock, defaulting to time.Now.
func (h *hostSampler) clockNow() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

type cpuTicks struct{ idleAll, total uint64 }

type diskCounters struct {
	device                string
	readBytes, writeBytes uint64
}

type netCounters struct{ rxBytes, txBytes uint64 }

// sample reads the current /proc counters and returns the deltas derivable
// against the previous reading. modelsRoot picks which diskstats row to
// track; empty or unresolvable simply skips the disk series.
func (h *hostSampler) sample(node, modelsRoot string, now int64) []proto.MetricSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []proto.MetricSample

	if cur, ok := readCPUTicks(h.procRoot); ok {
		if h.haveCPU {
			idleDelta := subUint64(cur.idleAll, h.prevCPU.idleAll)
			totalDelta := subUint64(cur.total, h.prevCPU.total)
			if totalDelta > 0 {
				busyPct := (1 - float64(idleDelta)/float64(totalDelta)) * 100
				out = append(out, proto.MetricSample{
					Metric: "gpuworks_node_cpu_busy_percent",
					Labels: map[string]string{"node": node}, Value: busyPct, TimeMS: now,
				})
			}
		}
		h.prevCPU, h.haveCPU = cur, true
	}

	if modelsRoot != "" {
		if major, minor, ok := resolveDeviceNumber(modelsRoot); ok {
			if cur, ok := readDiskCounters(h.procRoot, major, minor); ok {
				at := h.clockNow()
				if h.haveDisk && h.prevDisk.device == cur.device {
					if dt := at.Sub(h.prevDiskAt).Seconds(); dt > 0 {
						lbl := map[string]string{"node": node, "device": cur.device}
						out = append(out,
							proto.MetricSample{Metric: "gpuworks_node_disk_read_bytes_per_sec", Labels: lbl, Value: float64(subUint64(cur.readBytes, h.prevDisk.readBytes)) / dt, TimeMS: now},
							proto.MetricSample{Metric: "gpuworks_node_disk_write_bytes_per_sec", Labels: lbl, Value: float64(subUint64(cur.writeBytes, h.prevDisk.writeBytes)) / dt, TimeMS: now},
						)
					}
				}
				h.prevDisk, h.prevDiskAt, h.haveDisk = cur, at, true
			}
		}
	}

	if cur, ok := readNetCounters(h.procRoot); ok {
		at := h.clockNow()
		if h.haveNet {
			if dt := at.Sub(h.prevNetAt).Seconds(); dt > 0 {
				lbl := map[string]string{"node": node}
				out = append(out,
					proto.MetricSample{Metric: "gpuworks_node_net_rx_bytes_per_sec", Labels: lbl, Value: float64(subUint64(cur.rxBytes, h.prevNet.rxBytes)) / dt, TimeMS: now},
					proto.MetricSample{Metric: "gpuworks_node_net_tx_bytes_per_sec", Labels: lbl, Value: float64(subUint64(cur.txBytes, h.prevNet.txBytes)) / dt, TimeMS: now},
				)
			}
		}
		h.prevNet, h.prevNetAt, h.haveNet = cur, at, true
	}

	if h.sysRoot != "" {
		out = append(out, h.links.sample(h.procRoot, h.sysRoot, node, h.clockNow(), now)...)
	}
	return out
}

// readCPUTicks parses the aggregate "cpu " line of /proc/stat. idleAll and
// total follow the convention most monitoring tools use (idle+iowait vs. the
// sum of every field except guest/guest_nice, which are already included in
// user/nice and would otherwise be double-counted).
func readCPUTicks(procRoot string) (cpuTicks, bool) {
	f, err := os.Open(procRoot + "/stat")
	if err != nil {
		return cpuTicks{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		vals := make([]uint64, len(fields)-1)
		for i, s := range fields[1:] {
			vals[i], _ = strconv.ParseUint(s, 10, 64)
		}
		var idle, iowait uint64
		idle = vals[3]
		if len(vals) > 4 {
			iowait = vals[4]
		}
		var total uint64
		for _, v := range vals {
			total += v
		}
		return cpuTicks{idleAll: idle + iowait, total: total}, true
	}
	return cpuTicks{}, false
}

// resolveDeviceNumber resolves the block device (major, minor) backing path
// via stat(2), using the same 64-bit dev_t decomposition glibc's
// major()/minor() macros use. It never reads /proc itself: the result is
// matched against /proc/diskstats' own major/minor columns by the caller, so
// no device-name mapping step (and no extra failure mode) is needed here.
func resolveDeviceNumber(path string) (major, minor uint32, ok bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	st, ok2 := info.Sys().(*syscall.Stat_t)
	if !ok2 {
		return 0, 0, false
	}
	dev := uint64(st.Dev)
	major = uint32((dev>>8)&0xfff) | uint32((dev>>32)&0xfffff000)
	minor = uint32(dev&0xff) | uint32((dev>>12)&0xffffff00)
	return major, minor, true
}

// readDiskCounters scans /proc/diskstats for the row matching (major, minor)
// and returns its read/write byte counters (sectors are always 512 bytes,
// regardless of the device's logical block size — the kernel's own
// convention for this file).
func readDiskCounters(procRoot string, major, minor uint32) (diskCounters, bool) {
	f, err := os.Open(procRoot + "/diskstats")
	if err != nil {
		return diskCounters{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 14 {
			continue
		}
		fMajor, err1 := strconv.ParseUint(fields[0], 10, 32)
		fMinor, err2 := strconv.ParseUint(fields[1], 10, 32)
		if err1 != nil || err2 != nil || uint32(fMajor) != major || uint32(fMinor) != minor {
			continue
		}
		sectorsRead, errR := strconv.ParseUint(fields[5], 10, 64)
		sectorsWritten, errW := strconv.ParseUint(fields[9], 10, 64)
		if errR != nil || errW != nil {
			return diskCounters{}, false
		}
		return diskCounters{device: fields[2], readBytes: sectorsRead * 512, writeBytes: sectorsWritten * 512}, true
	}
	return diskCounters{}, false
}

// virtualInterfacePrefixes are container/virtualization plumbing whose
// traffic is either a loop back through the host (docker0/br-*/virbr-*/
// cni*/flannel*) or a duplicate of a physical interface's own traffic
// (veth*/tap*/tun*): counting them inflates the host network rate 2-3x on
// any node running containers (which is every gpuworks node). "lo" is exact,
// the rest are prefixes since the kernel appends a numeric/hash suffix.
var virtualInterfacePrefixes = []string{"veth", "docker", "br-", "virbr", "cni", "flannel", "tap", "tun"}

func isVirtualInterface(iface string) bool {
	if iface == "lo" {
		return true
	}
	for _, prefix := range virtualInterfacePrefixes {
		if strings.HasPrefix(iface, prefix) {
			return true
		}
	}
	return false
}

// readNetCounters sums rx/tx bytes from /proc/net/dev over every physical/
// external interface, excluding loopback and container/virtualization
// plumbing (see virtualInterfacePrefixes) that would otherwise double- or
// triple-count the same traffic.
func readNetCounters(procRoot string) (netCounters, bool) {
	f, err := os.Open(procRoot + "/net/dev")
	if err != nil {
		return netCounters{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var out netCounters
	seen := false
	for sc.Scan() {
		line := sc.Text()
		iface, rest, found := strings.Cut(line, ":")
		if !found {
			continue // header lines carry no colon
		}
		iface = strings.TrimSpace(iface)
		if iface == "" || isVirtualInterface(iface) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, errR := strconv.ParseUint(fields[0], 10, 64)
		tx, errT := strconv.ParseUint(fields[8], 10, 64)
		if errR != nil || errT != nil {
			continue
		}
		out.rxBytes += rx
		out.txBytes += tx
		seen = true
	}
	return out, seen
}
