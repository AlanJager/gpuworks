//go:build linux

// Per-NIC and per-RDMA-port traffic for the network monitor. Like the rest of
// the host sampler these are plain file reads (/proc/net/dev and sysfs); the
// node-wide net rate in hostmetrics_linux.go stays as it is.
package collector

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// linkNoiseCounters are the /proc/net/dev fields reported only when they move,
// by their column in the line after the interface name.
var linkNoiseCounters = []struct {
	kind  string
	field int
}{{"rx_errors", 2}, {"rx_dropped", 3}, {"tx_errors", 10}, {"tx_dropped", 11}}

// rdmaEventCounters are RDMA port counters that are zero on a healthy link:
// errors under counters/, RoCE congestion under hw_counters/. They are
// reported only when they move so a node with many ports adds no series while
// it is healthy.
var rdmaEventCounters = []string{
	"counters/symbol_error", "counters/link_downed", "counters/port_rcv_errors",
	"counters/port_xmit_discards", "counters/excessive_buffer_overrun_errors",
	"hw_counters/np_cnp_sent", "hw_counters/rp_cnp_handled", "hw_counters/out_of_buffer",
}

// linkReading is one sampling of every tracked NIC and RDMA port; values are
// cumulative counters keyed by "<link>/<counter>".
type linkReading struct {
	at     time.Time
	values map[string]uint64
}

// linkSampler keeps the previous reading so the next one can be turned into
// rates. A link that appears or a counter that resets simply skips a tick.
type linkSampler struct {
	prev *linkReading
}

// sample reads the NICs listed in procRoot/net/dev that are real endpoints
// and the ACTIVE RDMA ports under sysRoot, and returns their rates since the
// previous call.
func (l *linkSampler) sample(procRoot, sysRoot, node string, at time.Time, now int64) []proto.MetricSample {
	cur := &linkReading{at: at, values: map[string]uint64{}}
	nics := readLinkCounters(procRoot, sysRoot, cur.values)
	ports := readRDMACounters(sysRoot, cur.values)
	prev := l.prev
	l.prev = cur
	if prev == nil {
		return nil
	}
	dt := at.Sub(prev.at).Seconds()
	if dt <= 0 {
		return nil
	}
	rate := func(key string) (float64, bool) {
		before, ok := prev.values[key]
		after, ok2 := cur.values[key]
		if !ok || !ok2 {
			return 0, false
		}
		return float64(subUint64(after, before)) / dt, true
	}
	var out []proto.MetricSample
	for _, nic := range nics {
		labels := map[string]string{"node": node, "iface": nic}
		if v, ok := rate(nic + "/rx_bytes"); ok {
			out = append(out, proto.MetricSample{Metric: "gpuworks_node_nic_rx_bytes_per_sec", Labels: labels, Value: v, TimeMS: now})
		}
		if v, ok := rate(nic + "/tx_bytes"); ok {
			out = append(out, proto.MetricSample{Metric: "gpuworks_node_nic_tx_bytes_per_sec", Labels: labels, Value: v, TimeMS: now})
		}
		for _, c := range linkNoiseCounters {
			if v, ok := rate(nic + "/" + c.kind); ok && v > 0 {
				out = append(out, proto.MetricSample{Metric: "gpuworks_node_nic_errors_per_sec", Labels: map[string]string{"node": node, "iface": nic, "kind": c.kind}, Value: v, TimeMS: now})
			}
		}
	}
	for _, port := range ports {
		labels := map[string]string{"node": node, "hca": port}
		// port_rcv_data / port_xmit_data count 4-byte words.
		if v, ok := rate(port + "/counters/port_rcv_data"); ok {
			out = append(out, proto.MetricSample{Metric: "gpuworks_node_rdma_rx_bytes_per_sec", Labels: labels, Value: v * 4, TimeMS: now})
		}
		if v, ok := rate(port + "/counters/port_xmit_data"); ok {
			out = append(out, proto.MetricSample{Metric: "gpuworks_node_rdma_tx_bytes_per_sec", Labels: labels, Value: v * 4, TimeMS: now})
		}
		for _, counter := range rdmaEventCounters {
			if v, ok := rate(port + "/" + counter); ok && v > 0 {
				out = append(out, proto.MetricSample{Metric: "gpuworks_node_rdma_events_per_sec", Labels: map[string]string{"node": node, "hca": port, "counter": filepath.Base(counter)}, Value: v, TimeMS: now})
			}
		}
	}
	return out
}

// readLinkCounters records rx/tx bytes and the noise counters of every
// interface that can carry node traffic, returning their names. It keeps
// physical NICs, bonds, bridges and VLANs, the same kinds the network
// inventory reports, and skips loopback and container plumbing.
func readLinkCounters(procRoot, sysRoot string, values map[string]uint64) []string {
	f, err := os.Open(filepath.Join(procRoot, "net", "dev"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var names []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || isVirtualInterface(name) || !trafficEndpoint(procRoot, sysRoot, name) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			continue
		}
		rx, errR := strconv.ParseUint(fields[0], 10, 64)
		tx, errT := strconv.ParseUint(fields[8], 10, 64)
		if errR != nil || errT != nil {
			continue
		}
		values[name+"/rx_bytes"], values[name+"/tx_bytes"] = rx, tx
		for _, c := range linkNoiseCounters {
			if v, err := strconv.ParseUint(fields[c.field], 10, 64); err == nil {
				values[name+"/"+c.kind] = v
			}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// trafficEndpoint reports whether an interface is a physical NIC, a bond, a
// bridge or a VLAN: tap devices, overlays and the like either duplicate one
// of those or carry no node traffic.
func trafficEndpoint(procRoot, sysRoot, name string) bool {
	dir := filepath.Join(sysRoot, "class", "net", name)
	for _, marker := range []string{"device", "bonding", "bridge"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(procRoot, "net", "vlan", name))
	return err == nil
}

// readRDMACounters records the data and event counters of every ACTIVE RDMA
// port, returning the ports as "device:port".
func readRDMACounters(sysRoot string, values map[string]uint64) []string {
	root := filepath.Join(sysRoot, "class", "infiniband")
	devices, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var ports []string
	for _, device := range devices {
		portEntries, err := os.ReadDir(filepath.Join(root, device.Name(), "ports"))
		if err != nil {
			continue
		}
		for _, portEntry := range portEntries {
			dir := filepath.Join(root, device.Name(), "ports", portEntry.Name())
			if !strings.HasSuffix(readTrimmed(filepath.Join(dir, "state")), "ACTIVE") {
				continue
			}
			port := device.Name() + ":" + portEntry.Name()
			found := false
			for _, counter := range append([]string{"counters/port_rcv_data", "counters/port_xmit_data"}, rdmaEventCounters...) {
				if v, err := strconv.ParseUint(readTrimmed(filepath.Join(dir, counter)), 10, 64); err == nil {
					values[port+"/"+counter] = v
					found = true
				}
			}
			if found {
				ports = append(ports, port)
			}
		}
	}
	sort.Strings(ports)
	return ports
}

func readTrimmed(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
