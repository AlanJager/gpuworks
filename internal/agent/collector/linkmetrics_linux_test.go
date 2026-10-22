//go:build linux

package collector

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// netDevLine is one /proc/net/dev row: rx bytes packets errs drop fifo frame
// compressed multicast, then tx bytes packets errs drop fifo colls carrier
// compressed.
func netDevLine(name string, rx, rxErr, rxDrop, tx, txErr, txDrop uint64) string {
	return fmt.Sprintf("%6s: %d 100 %d %d 0 0 0 0 %d 90 %d %d 0 0 0 0\n", name, rx, rxErr, rxDrop, tx, txErr, txDrop)
}

const netDevHeader = "Inter-|   Receive                                                |  Transmit\n" +
	" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"

type linkFixture struct {
	t              *testing.T
	proc, sys      string
	sampler        *hostSampler
	clock          time.Time
	sampleSequence int64
}

func newLinkFixture(t *testing.T) *linkFixture {
	f := &linkFixture{t: t, proc: t.TempDir(), sys: t.TempDir(), clock: time.Unix(1_000, 0)}
	f.sampler = &hostSampler{procRoot: f.proc, sysRoot: f.sys, now: func() time.Time { return f.clock }}
	// A physical NIC, a bond, a bridge and a VLAN are endpoints; a tap
	// device and container plumbing are not.
	writeProcFile(t, f.sys, "class/net/eth0/device/vendor", "0x15b3\n")
	writeProcFile(t, f.sys, "class/net/bond0/bonding/mode", "802.3ad 4\n")
	writeProcFile(t, f.sys, "class/net/br0/bridge/stp_state", "0\n")
	writeProcFile(t, f.proc, "net/vlan/eth0.100", "eth0.100  VID: 100\n")
	writeProcFile(t, f.sys, "class/net/tap0/tun_flags", "0x1002\n")
	return f
}

func (f *linkFixture) netDev(lines ...string) {
	writeProcFile(f.t, f.proc, "net/dev", netDevHeader+strings.Join(lines, ""))
}

func (f *linkFixture) rdmaPort(device, port, state string, counters map[string]uint64) {
	dir := filepath.Join("class/infiniband", device, "ports", port)
	writeProcFile(f.t, f.sys, filepath.Join(dir, "state"), state+"\n")
	for name, value := range counters {
		writeProcFile(f.t, f.sys, filepath.Join(dir, name), fmt.Sprintf("%d\n", value))
	}
}

// tick advances the clock by ten seconds and returns the link series only.
func (f *linkFixture) tick() map[string]float64 {
	f.clock = f.clock.Add(10 * time.Second)
	f.sampleSequence++
	out := map[string]float64{}
	for _, s := range f.sampler.sample("node-1", "", f.sampleSequence) {
		if !strings.HasPrefix(s.Metric, "gpuworks_node_nic_") && !strings.HasPrefix(s.Metric, "gpuworks_node_rdma_") {
			continue
		}
		if s.Labels["node"] != "node-1" || s.TimeMS != f.sampleSequence {
			f.t.Fatalf("sample labels/time = %+v", s)
		}
		out[seriesKey(s)] = s.Value
	}
	return out
}

func seriesKey(s proto.MetricSample) string {
	var parts []string
	for k, v := range s.Labels {
		if k != "node" {
			parts = append(parts, k+"="+v)
		}
	}
	sort.Strings(parts)
	return s.Metric + "{" + strings.Join(parts, ",") + "}"
}

func TestLinkSamplerRatesPerNICAndRDMAPort(t *testing.T) {
	f := newLinkFixture(t)
	ib := map[string]uint64{"counters/port_rcv_data": 1_000, "counters/port_xmit_data": 2_000, "counters/symbol_error": 3, "counters/link_downed": 1}
	roce := map[string]uint64{"counters/port_rcv_data": 0, "counters/port_xmit_data": 0, "hw_counters/np_cnp_sent": 10, "hw_counters/out_of_buffer": 0}
	f.rdmaPort("mlx5_0", "1", "4: ACTIVE", ib)
	f.rdmaPort("mlx5_1", "1", "1: DOWN", map[string]uint64{"counters/port_rcv_data": 5, "counters/port_xmit_data": 5})
	f.rdmaPort("mlx5_2", "1", "4: ACTIVE", roce)
	f.netDev(
		netDevLine("lo", 9_000, 0, 0, 9_000, 0, 0),
		netDevLine("eth0", 1_000, 0, 0, 2_000, 0, 0),
		netDevLine("bond0", 0, 0, 0, 0, 0, 0),
		netDevLine("br0", 0, 0, 0, 0, 0, 0),
		netDevLine("eth0.100", 0, 0, 0, 0, 0, 0),
		netDevLine("tap0", 0, 0, 0, 0, 0, 0),
		netDevLine("veth12ab", 0, 0, 0, 0, 0, 0),
	)
	if first := f.tick(); len(first) != 0 {
		t.Fatalf("first tick has no previous reading: %v", first)
	}

	f.netDev(
		netDevLine("lo", 99_000, 0, 0, 99_000, 0, 0),
		netDevLine("eth0", 101_000, 5, 0, 52_000, 0, 20),
		netDevLine("bond0", 10_000, 0, 0, 10_000, 0, 0),
		netDevLine("br0", 0, 0, 0, 0, 0, 0),
		netDevLine("eth0.100", 0, 0, 0, 0, 0, 0),
		netDevLine("tap0", 5_000, 0, 0, 5_000, 0, 0),
		netDevLine("veth12ab", 5_000, 0, 0, 5_000, 0, 0),
	)
	ib["counters/port_rcv_data"], ib["counters/port_xmit_data"], ib["counters/symbol_error"] = 1_000+250_000, 2_000+500_000, 13
	roce["counters/port_xmit_data"], roce["hw_counters/np_cnp_sent"] = 25, 40
	f.rdmaPort("mlx5_0", "1", "4: ACTIVE", ib)
	f.rdmaPort("mlx5_2", "1", "4: ACTIVE", roce)
	got := f.tick()
	want := map[string]float64{
		"gpuworks_node_nic_rx_bytes_per_sec{iface=eth0}":                       10_000,
		"gpuworks_node_nic_tx_bytes_per_sec{iface=eth0}":                       5_000,
		"gpuworks_node_nic_errors_per_sec{iface=eth0,kind=rx_errors}":          0.5,
		"gpuworks_node_nic_errors_per_sec{iface=eth0,kind=tx_dropped}":         2,
		"gpuworks_node_nic_rx_bytes_per_sec{iface=bond0}":                      1_000,
		"gpuworks_node_nic_tx_bytes_per_sec{iface=bond0}":                      1_000,
		"gpuworks_node_nic_rx_bytes_per_sec{iface=br0}":                        0,
		"gpuworks_node_nic_tx_bytes_per_sec{iface=br0}":                        0,
		"gpuworks_node_nic_rx_bytes_per_sec{iface=eth0.100}":                   0,
		"gpuworks_node_nic_tx_bytes_per_sec{iface=eth0.100}":                   0,
		"gpuworks_node_rdma_rx_bytes_per_sec{hca=mlx5_0:1}":                    100_000, // 250k words × 4 B / 10 s
		"gpuworks_node_rdma_tx_bytes_per_sec{hca=mlx5_0:1}":                    200_000,
		"gpuworks_node_rdma_events_per_sec{counter=symbol_error,hca=mlx5_0:1}": 1,
		"gpuworks_node_rdma_rx_bytes_per_sec{hca=mlx5_2:1}":                    0,
		"gpuworks_node_rdma_tx_bytes_per_sec{hca=mlx5_2:1}":                    10,
		"gpuworks_node_rdma_events_per_sec{counter=np_cnp_sent,hca=mlx5_2:1}":  3,
	}
	if len(got) != len(want) {
		t.Fatalf("series = %v\nwant %v", got, want)
	}
	for key, value := range want {
		if g, ok := got[key]; !ok || g < value-1e-9 || g > value+1e-9 {
			t.Fatalf("%s = %v (present %v), want %v\nall: %v", key, g, ok, value, got)
		}
	}
}

func TestLinkSamplerSkipsResetsAndNewLinks(t *testing.T) {
	f := newLinkFixture(t)
	f.rdmaPort("mlx5_0", "1", "4: ACTIVE", map[string]uint64{"counters/port_rcv_data": 1_000_000, "counters/port_xmit_data": 1_000_000, "counters/port_rcv_errors": 7})
	f.netDev(netDevLine("eth0", 1_000_000, 4, 0, 1_000_000, 0, 0))
	f.tick()

	// The host rebooted: counters restart from zero, and a NIC that was not
	// there before (a hotplugged bond) has no previous reading.
	f.rdmaPort("mlx5_0", "1", "4: ACTIVE", map[string]uint64{"counters/port_rcv_data": 10, "counters/port_xmit_data": 10, "counters/port_rcv_errors": 0})
	f.netDev(netDevLine("eth0", 500, 0, 0, 500, 0, 0), netDevLine("bond0", 100, 0, 0, 100, 0, 0))
	got := f.tick()
	want := map[string]float64{
		"gpuworks_node_nic_rx_bytes_per_sec{iface=eth0}":    0,
		"gpuworks_node_nic_tx_bytes_per_sec{iface=eth0}":    0,
		"gpuworks_node_rdma_rx_bytes_per_sec{hca=mlx5_0:1}": 0,
		"gpuworks_node_rdma_tx_bytes_per_sec{hca=mlx5_0:1}": 0,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after reset = %v, want %v", got, want)
	}

	// The port going down drops its series; the new bond now has a rate.
	f.rdmaPort("mlx5_0", "1", "1: DOWN", nil)
	f.netDev(netDevLine("eth0", 500, 0, 0, 500, 0, 0), netDevLine("bond0", 1_100, 0, 0, 100, 0, 0))
	got = f.tick()
	if _, ok := got["gpuworks_node_rdma_rx_bytes_per_sec{hca=mlx5_0:1}"]; ok || got["gpuworks_node_nic_rx_bytes_per_sec{iface=bond0}"] != 100 {
		t.Fatalf("third tick = %v", got)
	}
}

func TestLinkSamplerWithoutSysfsReportsNothing(t *testing.T) {
	procRoot := t.TempDir()
	writeProcFile(t, procRoot, "net/dev", netDevHeader+netDevLine("eth0", 1, 0, 0, 1, 0, 0))
	h := &hostSampler{procRoot: procRoot, sysRoot: filepath.Join(t.TempDir(), "missing")}
	h.sample("node-1", "", 1)
	for _, s := range h.sample("node-1", "", 2) {
		if strings.HasPrefix(s.Metric, "gpuworks_node_nic_") || strings.HasPrefix(s.Metric, "gpuworks_node_rdma_") {
			t.Fatalf("no sysfs means no endpoint can be identified: %+v", s)
		}
	}
}
