package collector

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/hostsim"
	"gpuworks.ai/gpuworks/internal/proto"
)

// sampledHost is a simulated machine that reports its own CPU, disk and
// network figures and memory (hostsim.Host), recording who asked.
type sampledHost struct {
	nodes []string
}

func (s *sampledHost) host() *hostsim.Host {
	return &hostsim.Host{
		Describe: func(sys proto.SysInfo) proto.SysInfo {
			sys.MemTotalKB, sys.MemAvailKB = 1024<<20, 768<<20
			return sys
		},
		Samples: func(node string, now int64) []proto.MetricSample {
			s.nodes = append(s.nodes, node)
			return []proto.MetricSample{
				{Metric: "gpuworks_node_cpu_busy_percent", Labels: map[string]string{"node": node}, Value: 42, TimeMS: now},
				{Metric: "gpuworks_node_nic_rx_bytes_per_sec", Labels: map[string]string{"node": node, "iface": "eth0"}, Value: 7, TimeMS: now},
			}
		},
	}
}

func TestSimulatedHostSamplesReplaceTheRealHosts(t *testing.T) {
	var mu sync.Mutex
	var got []proto.MetricSample
	prober := &sampledHost{}
	c := New("node-1", &collectorProber{err: errors.New("smi failed")}, func() []RunningInstance { return nil },
		func(b proto.MetricBatch) { mu.Lock(); got = b.Samples; mu.Unlock() }, slog.Default())
	c.SetModelsRoot(t.TempDir())
	c.SetSimulatedHost(prober.host())
	c.RunOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(prober.nodes) != 1 || prober.nodes[0] != "node-1" {
		t.Fatalf("host samples asked for %v", prober.nodes)
	}
	seen := map[string]proto.MetricSample{}
	for _, s := range got {
		if s.Labels["device"] != "" || (s.Labels["iface"] != "" && s.Labels["iface"] != "eth0") {
			t.Fatalf("a real host device leaked: %+v", s)
		}
		seen[s.Metric] = s
	}
	if seen["gpuworks_node_cpu_busy_percent"].Value != 42 || seen["gpuworks_node_nic_rx_bytes_per_sec"].Value != 7 {
		t.Fatalf("simulated samples missing: %+v", got)
	}
	if seen["gpuworks_node_mem_total_bytes"].Value != float64(1024<<30) || seen["gpuworks_node_mem_used_bytes"].Value != float64(256<<30) {
		t.Fatalf("memory is not the simulated machine's: %+v", got)
	}
	for metric := range seen {
		switch metric {
		case "gpuworks_node_disk_read_bytes_per_sec", "gpuworks_node_disk_write_bytes_per_sec", "gpuworks_node_net_rx_bytes_per_sec":
			t.Fatalf("the real host's %s was sampled", metric)
		}
	}
}
