// Host CPU/disk/network rate sampling for macOS nodes. macOS has no /proc and
// its CPU and disk counters sit behind Mach and IOKit calls that need cgo, so
// the sources differ from Linux: CPU comes from one long-lived `iostat`, disk
// counters from one `ioreg` per tick (the same kind of call the Apple GPU
// probe makes), and network counters from the routing sysctl. The sampler and
// the parsers carry no build tag so they are tested on any host;
// hostmetrics_darwin.go wires the real sources.
package collector

import (
	"bufio"
	"encoding/binary"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// macDiskIO is one physical disk's cumulative byte counters.
type macDiskIO struct{ readBytes, writeBytes uint64 }

// macIfCounters is one interface's cumulative counters.
type macIfCounters struct {
	rxBytes, txBytes            uint64
	rxErrors, txErrors, rxDrops uint64
}

// macSampler turns the macOS sources into the same series the Linux sampler
// emits. Rates need two readings, so a source's first reading emits nothing.
type macSampler struct {
	// cpuBusy returns the latest CPU busy share and when it was measured.
	cpuBusy func() (float64, time.Time, bool)
	// disks returns cumulative counters per BSD disk name.
	disks func() (map[string]macDiskIO, error)
	// interfaces returns cumulative counters per interface name.
	interfaces func() (map[string]macIfCounters, error)
	now        func() time.Time

	mu         sync.Mutex
	prevDisk   map[string]macDiskIO
	prevDiskAt time.Time
	prevNet    map[string]macIfCounters
	prevNetAt  time.Time
}

// macIostatInterval is how often the long-lived iostat reports CPU usage. It
// matches the collector's tick.
const macIostatInterval = 10 * time.Second

// cpuStaleAfter bounds how old a CPU reading may be and still be reported:
// two iostat intervals, so one late line does not drop a point.
const cpuStaleAfter = 2*macIostatInterval + 5*time.Second

func (h *macSampler) clockNow() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (h *macSampler) sample(node, _ string, now int64) []proto.MetricSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []proto.MetricSample
	at := h.clockNow()
	nodeLabel := map[string]string{"node": node}

	if h.cpuBusy != nil {
		if busy, measured, ok := h.cpuBusy(); ok && at.Sub(measured) <= cpuStaleAfter {
			out = append(out, proto.MetricSample{Metric: "gpuworks_node_cpu_busy_percent", Labels: nodeLabel, Value: busy, TimeMS: now})
		}
	}

	if h.disks != nil {
		if cur, err := h.disks(); err == nil {
			if dt := at.Sub(h.prevDiskAt).Seconds(); h.prevDisk != nil && dt > 0 {
				for _, name := range sortedKeys(cur) {
					prev, ok := h.prevDisk[name]
					if !ok {
						continue
					}
					lbl := map[string]string{"node": node, "device": name}
					out = append(out,
						proto.MetricSample{Metric: "gpuworks_node_disk_read_bytes_per_sec", Labels: lbl, Value: float64(subUint64(cur[name].readBytes, prev.readBytes)) / dt, TimeMS: now},
						proto.MetricSample{Metric: "gpuworks_node_disk_write_bytes_per_sec", Labels: lbl, Value: float64(subUint64(cur[name].writeBytes, prev.writeBytes)) / dt, TimeMS: now},
					)
				}
			}
			h.prevDisk, h.prevDiskAt = cur, at
		}
	}

	if h.interfaces != nil {
		if cur, err := h.interfaces(); err == nil {
			if dt := at.Sub(h.prevNetAt).Seconds(); h.prevNet != nil && dt > 0 {
				var rx, tx float64
				var nics []proto.MetricSample
				for _, name := range sortedKeys(cur) {
					prev, ok := h.prevNet[name]
					if !ok {
						continue
					}
					c := cur[name]
					r := float64(subUint64(c.rxBytes, prev.rxBytes)) / dt
					t := float64(subUint64(c.txBytes, prev.txBytes)) / dt
					rx, tx = rx+r, tx+t
					lbl := map[string]string{"node": node, "iface": name}
					nics = append(nics,
						proto.MetricSample{Metric: "gpuworks_node_nic_rx_bytes_per_sec", Labels: lbl, Value: r, TimeMS: now},
						proto.MetricSample{Metric: "gpuworks_node_nic_tx_bytes_per_sec", Labels: lbl, Value: t, TimeMS: now},
					)
					for _, e := range []struct {
						kind      string
						cur, prev uint64
					}{{"rx_errors", c.rxErrors, prev.rxErrors}, {"tx_errors", c.txErrors, prev.txErrors}, {"rx_dropped", c.rxDrops, prev.rxDrops}} {
						if v := float64(subUint64(e.cur, e.prev)) / dt; v > 0 {
							nics = append(nics, proto.MetricSample{Metric: "gpuworks_node_nic_errors_per_sec", Labels: map[string]string{"node": node, "iface": name, "kind": e.kind}, Value: v, TimeMS: now})
						}
					}
				}
				out = append(out,
					proto.MetricSample{Metric: "gpuworks_node_net_rx_bytes_per_sec", Labels: nodeLabel, Value: rx, TimeMS: now},
					proto.MetricSample{Metric: "gpuworks_node_net_tx_bytes_per_sec", Labels: nodeLabel, Value: tx, TimeMS: now},
				)
				out = append(out, nics...)
			}
			h.prevNet, h.prevNetAt = cur, at
		}
	}
	return out
}

// parseIostatCPU reads one data line of `iostat -n 0 -C`: "us sy id 1m 5m
// 15m". Header lines (which iostat repeats) are not data.
func parseIostatCPU(line string) (busy float64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return 0, false
	}
	idle, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return 0, false
	}
	for _, f := range fields[:2] {
		if _, err := strconv.ParseFloat(f, 64); err != nil {
			return 0, false
		}
	}
	if idle < 0 || idle > 100 {
		return 0, false
	}
	return 100 - idle, true
}

// parseBlockStorage reads `ioreg -r -c IOBlockStorageDriver -d 2 -l`: each
// driver's Statistics, keyed by the BSD name of the disk under it. Disk images
// (mounted .dmg files) are skipped: their reads are reads of a file on a real
// disk, which that disk already counts. A driver with no disk under it (an
// empty card reader) has nothing to report.
func parseBlockStorage(out string) map[string]macDiskIO {
	disks := map[string]macDiskIO{}
	var stats *macDiskIO
	bsd, image := "", false
	flush := func() {
		if stats != nil && bsd != "" && !image {
			disks[bsd] = *stats
		}
		stats, bsd, image = nil, "", false
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimLeft(sc.Text(), " |")
		switch {
		case strings.HasPrefix(line, "+-o ") && strings.Contains(line, "<class IOBlockStorageDriver"):
			flush()
		case strings.HasPrefix(line, "+-o ") && strings.Contains(line, "<class IOMedia"):
			image = strings.Contains(line, "Disk Image")
		case strings.HasPrefix(line, `"Statistics" = {`):
			s := macDiskIO{}
			for _, kv := range strings.Split(strings.Trim(strings.TrimPrefix(line, `"Statistics" = `), "{}"), ",") {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
				if err != nil {
					continue
				}
				switch strings.Trim(k, `"`) {
				case "Bytes (Read)":
					s.readBytes = n
				case "Bytes (Write)":
					s.writeBytes = n
				}
			}
			stats = &s
		case strings.HasPrefix(line, `"BSD Name" = "`) && bsd == "":
			bsd = strings.TrimSuffix(strings.TrimPrefix(line, `"BSD Name" = "`), `"`)
		}
	}
	flush()
	return disks
}

// The routing sysctl's NET_RT_IFLIST2 answer is a run of messages, each
// starting with msglen (uint16) and type (byte 3). An RTM_IFINFO2 message is
// struct if_msghdr2: the interface index at offset 12 and struct if_data64 at
// offset 32 (darwin/arm64 and amd64 share the layout).
const (
	rtmIfinfo2         = 0x12
	ifMsghdr2Index     = 12
	ifMsghdr2Data      = 32
	ifData64Ierrors    = 32
	ifData64Oerrors    = 48
	ifData64Ibytes     = 64
	ifData64Obytes     = 72
	ifData64Iqdrops    = 96
	ifMsghdr2MinLength = ifMsghdr2Data + 128
)

// parseIfList2 reads the counters of every interface in a NET_RT_IFLIST2
// answer, keyed by interface index.
func parseIfList2(buf []byte) map[uint16]macIfCounters {
	out := map[uint16]macIfCounters{}
	le := binary.LittleEndian
	for len(buf) >= 4 {
		msglen := int(le.Uint16(buf))
		if msglen < 4 || msglen > len(buf) {
			break
		}
		msg := buf[:msglen]
		buf = buf[msglen:]
		if msg[3] != rtmIfinfo2 || len(msg) < ifMsghdr2MinLength {
			continue
		}
		data := msg[ifMsghdr2Data:]
		out[le.Uint16(msg[ifMsghdr2Index:])] = macIfCounters{
			rxBytes:  le.Uint64(data[ifData64Ibytes:]),
			txBytes:  le.Uint64(data[ifData64Obytes:]),
			rxErrors: le.Uint64(data[ifData64Ierrors:]),
			txErrors: le.Uint64(data[ifData64Oerrors:]),
			rxDrops:  le.Uint64(data[ifData64Iqdrops:]),
		}
	}
	return out
}

// isMacTrafficInterface keeps a Mac's Ethernet and Wi-Fi ports (en*). The
// rest are loopback, tunnels (utun), Apple peer-to-peer links (awdl, llw),
// bridges over en ports that already count the traffic, and VM plumbing.
func isMacTrafficInterface(name string) bool {
	return strings.HasPrefix(name, "en")
}
