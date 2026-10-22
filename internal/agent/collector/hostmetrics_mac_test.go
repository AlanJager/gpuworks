package collector

import (
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestParseIostatCPUReadsDataLinesOnly(t *testing.T) {
	raw, err := os.ReadFile("testdata/darwin-iostat-cpu.txt")
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	for _, line := range strings.Split(string(raw), "\n") {
		if busy, ok := parseIostatCPU(line); ok {
			got = append(got, busy)
		}
	}
	// M4 Max, `iostat -n 0 -C -w 1 -c 3`: two header lines, three reports.
	want := []float64{5, 4, 9}
	if len(got) != len(want) {
		t.Fatalf("busy = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("busy = %v, want %v", got, want)
		}
	}
	for _, line := range []string{"", "      cpu    load average", " us sy id   1m   5m   15m", "3 2 101 1 1 1", "x y z"} {
		if _, ok := parseIostatCPU(line); ok {
			t.Errorf("parseIostatCPU(%q) accepted a non-data line", line)
		}
	}
}

func TestParseBlockStorageKeepsPhysicalDisks(t *testing.T) {
	raw, err := os.ReadFile("testdata/darwin-ioreg-blockstorage.txt")
	if err != nil {
		t.Fatal(err)
	}
	disks := parseBlockStorage(string(raw))
	// The fixture holds an empty driver (no disk under it), the internal SSD
	// (disk0) and a mounted disk image (disk5).
	if len(disks) != 1 {
		t.Fatalf("disks = %+v, want only disk0", disks)
	}
	d, ok := disks["disk0"]
	if !ok || d.readBytes == 0 || d.writeBytes == 0 {
		t.Fatalf("disk0 = %+v (present %v)", d, ok)
	}
	if !strings.Contains(string(raw), `"Bytes (Read)"=`+strconv.FormatUint(d.readBytes, 10)) || !strings.Contains(string(raw), `"Bytes (Write)"=`+strconv.FormatUint(d.writeBytes, 10)) {
		t.Fatalf("disk0 counters %+v do not match the fixture", d)
	}
	if got := parseBlockStorage(""); len(got) != 0 {
		t.Fatalf("empty output = %+v", got)
	}
}

// ifInfo2 builds one RTM_IFINFO2 message the way the kernel lays it out.
func ifInfo2(index uint16, rx, tx, rxErr, txErr, drops uint64) []byte {
	msg := make([]byte, ifMsghdr2MinLength)
	le := binary.LittleEndian
	le.PutUint16(msg, uint16(len(msg)))
	msg[2], msg[3] = 7, rtmIfinfo2
	le.PutUint16(msg[ifMsghdr2Index:], index)
	data := msg[ifMsghdr2Data:]
	le.PutUint64(data[ifData64Ibytes:], rx)
	le.PutUint64(data[ifData64Obytes:], tx)
	le.PutUint64(data[ifData64Ierrors:], rxErr)
	le.PutUint64(data[ifData64Oerrors:], txErr)
	le.PutUint64(data[ifData64Iqdrops:], drops)
	return msg
}

func TestParseIfList2ReadsInterfaceMessages(t *testing.T) {
	other := make([]byte, 20) // an RTM_NEWMADDR2 between interface messages
	binary.LittleEndian.PutUint16(other, 20)
	other[3] = 0x13
	buf := append(append(ifInfo2(4, 1000, 2000, 1, 2, 3), other...), ifInfo2(9, 5, 6, 0, 0, 0)...)
	buf = append(buf, 0xff, 0x00) // a truncated tail ends the walk
	got := parseIfList2(buf)
	if len(got) != 2 {
		t.Fatalf("interfaces = %+v", got)
	}
	if c := got[4]; c != (macIfCounters{rxBytes: 1000, txBytes: 2000, rxErrors: 1, txErrors: 2, rxDrops: 3}) {
		t.Fatalf("index 4 = %+v", c)
	}
	if c := got[9]; c.rxBytes != 5 || c.txBytes != 6 {
		t.Fatalf("index 9 = %+v", c)
	}
	short := ifInfo2(1, 1, 1, 0, 0, 0)[:40]
	binary.LittleEndian.PutUint16(short, 40)
	if got := parseIfList2(short); len(got) != 0 {
		t.Fatalf("a message shorter than if_msghdr2 was read: %+v", got)
	}
}

func TestMacTrafficInterfaces(t *testing.T) {
	for name, want := range map[string]bool{"en0": true, "en7": true, "lo0": false, "utun3": false, "awdl0": false, "llw0": false, "bridge0": false, "anpi0": false, "vmenet0": false} {
		if got := isMacTrafficInterface(name); got != want {
			t.Errorf("isMacTrafficInterface(%q) = %v, want %v", name, got, want)
		}
	}
}

func samplesByMetric(samples []proto.MetricSample) map[string][]proto.MetricSample {
	out := map[string][]proto.MetricSample{}
	for _, s := range samples {
		out[s.Metric] = append(out[s.Metric], s)
	}
	return out
}

func TestMacSamplerRates(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	cpuAt, cpuOK := clock, true
	disks := map[string]macDiskIO{"disk0": {readBytes: 1000, writeBytes: 5000}}
	nets := map[string]macIfCounters{"en0": {rxBytes: 100, txBytes: 200}}
	var diskErr error
	h := &macSampler{
		now:        func() time.Time { return clock },
		cpuBusy:    func() (float64, time.Time, bool) { return 12.5, cpuAt, cpuOK },
		disks:      func() (map[string]macDiskIO, error) { return disks, diskErr },
		interfaces: func() (map[string]macIfCounters, error) { return nets, nil },
	}

	first := samplesByMetric(h.sample("n1", "", 1))
	if len(first["gpuworks_node_cpu_busy_percent"]) != 1 || first["gpuworks_node_cpu_busy_percent"][0].Value != 12.5 {
		t.Fatalf("cpu on first tick = %+v", first["gpuworks_node_cpu_busy_percent"])
	}
	if len(first["gpuworks_node_disk_read_bytes_per_sec"])+len(first["gpuworks_node_net_rx_bytes_per_sec"]) != 0 {
		t.Fatalf("rates on the first reading: %+v", first)
	}

	clock = clock.Add(10 * time.Second)
	disks = map[string]macDiskIO{"disk0": {readBytes: 11000, writeBytes: 4000}, "disk4": {readBytes: 1}}
	nets = map[string]macIfCounters{"en0": {rxBytes: 1100, txBytes: 400, rxErrors: 5}, "en1": {rxBytes: 50}}
	second := samplesByMetric(h.sample("n1", "", 2))
	read := second["gpuworks_node_disk_read_bytes_per_sec"]
	if len(read) != 1 || read[0].Labels["device"] != "disk0" || read[0].Value != 1000 {
		t.Fatalf("disk read = %+v (a disk with no previous reading must wait a tick)", read)
	}
	if w := second["gpuworks_node_disk_write_bytes_per_sec"]; len(w) != 1 || w[0].Value != 0 {
		t.Fatalf("a counter that went backwards must read as no movement: %+v", w)
	}
	if rx := second["gpuworks_node_net_rx_bytes_per_sec"]; len(rx) != 1 || rx[0].Value != 100 {
		t.Fatalf("net rx = %+v", rx)
	}
	if tx := second["gpuworks_node_net_tx_bytes_per_sec"]; len(tx) != 1 || tx[0].Value != 20 {
		t.Fatalf("net tx = %+v", tx)
	}
	if nic := second["gpuworks_node_nic_rx_bytes_per_sec"]; len(nic) != 1 || nic[0].Labels["iface"] != "en0" {
		t.Fatalf("nic rx = %+v", nic)
	}
	if e := second["gpuworks_node_nic_errors_per_sec"]; len(e) != 1 || e[0].Labels["kind"] != "rx_errors" || e[0].Value != 0.5 {
		t.Fatalf("nic errors = %+v", e)
	}

	// A stale CPU reading (iostat stopped) and a failing disk source report
	// nothing rather than an old or invented value; the network still does.
	clock = clock.Add(10 * time.Second)
	cpuAt = clock.Add(-time.Minute)
	diskErr = errors.New("ioreg: exit status 1")
	third := samplesByMetric(h.sample("n1", "", 3))
	if len(third["gpuworks_node_cpu_busy_percent"]) != 0 {
		t.Fatalf("stale cpu reported: %+v", third["gpuworks_node_cpu_busy_percent"])
	}
	if len(third["gpuworks_node_disk_read_bytes_per_sec"]) != 0 {
		t.Fatalf("disk reported while its source fails: %+v", third)
	}
	if len(third["gpuworks_node_net_rx_bytes_per_sec"]) != 1 {
		t.Fatalf("net missing: %+v", third)
	}
	cpuOK = false
	if got := samplesByMetric(h.sample("n1", "", 4)); len(got["gpuworks_node_cpu_busy_percent"]) != 0 {
		t.Fatalf("cpu reported before any reading: %+v", got)
	}
}
