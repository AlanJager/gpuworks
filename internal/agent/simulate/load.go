package simulate

import (
	"math"
	"math/rand/v2"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Rhythm is the share of its peak load the simulated platform carries at t:
// a working day with a lunch dip and an evening tail, and nights and
// weekends that stay busy with batch work and other time zones. The nodes' live metrics and the experience
// environment's history follow the same curve, so the two meet.
func Rhythm(t time.Time) float64 {
	t = t.Local()
	h := float64(t.Hour()) + float64(t.Minute())/60
	workday := 0.08 + 0.92*math.Exp(-math.Pow((h-10.5)/2.2, 2)) + 0.95*math.Exp(-math.Pow((h-15.5)/2.6, 2)) -
		0.25*math.Exp(-math.Pow((h-12.5)/0.8, 2)) + 0.25*math.Exp(-math.Pow((h-20.5)/1.8, 2))
	workday = 0.28 + 0.72*math.Min(math.Max(workday, 0), 1)
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return workday * 0.6
	}
	return workday
}

// BusyUtilization is the mean utilization (percent) of a card an engine
// holds, at load share r: an inference engine idles low between requests
// and runs most of the card at the peak.
func BusyUtilization(r float64) float64 { return 28 + 66*r }

// HostSamples are the machine's CPU, disk and network figures, in place of
// the real host's (hostsim.Host.Samples): they rise with the load and the cards
// the node's instances hold.
func (p *Prober) HostSamples(node string, now int64) []proto.MetricSample {
	held := 0.0
	for i := 0; i < p.spec.Count; i++ {
		held += p.cards.share(i)
	}
	if p.spec.Count > 0 {
		held /= float64(p.spec.Count)
	}
	load := Rhythm(time.UnixMilli(now)) * held
	jitter := func(v float64) float64 { return math.Max(0, v*(0.85+rand.Float64()*0.3)) }
	const mb = 1 << 20
	rx, tx := jitter(2*mb+90*mb*load), jitter(1*mb+55*mb*load)
	node1 := map[string]string{"node": node}
	sample := func(metric string, labels map[string]string, v float64) proto.MetricSample {
		return proto.MetricSample{Metric: metric, Labels: labels, Value: math.Round(v), TimeMS: now}
	}
	return []proto.MetricSample{
		sample("gpuworks_node_cpu_busy_percent", node1, jitter(4+38*load)),
		sample("gpuworks_node_disk_read_bytes_per_sec", map[string]string{"node": node, "device": "nvme0n1"}, jitter(1*mb+24*mb*load)),
		sample("gpuworks_node_disk_write_bytes_per_sec", map[string]string{"node": node, "device": "nvme0n1"}, jitter(2*mb+9*mb*load)),
		sample("gpuworks_node_net_rx_bytes_per_sec", node1, rx),
		sample("gpuworks_node_net_tx_bytes_per_sec", node1, tx),
		sample("gpuworks_node_nic_rx_bytes_per_sec", map[string]string{"node": node, "iface": "eth0"}, rx),
		sample("gpuworks_node_nic_tx_bytes_per_sec", map[string]string{"node": node, "iface": "eth0"}, tx),
	}
}
