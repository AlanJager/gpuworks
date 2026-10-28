package simulate

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Prober reports the simulated machine's cards and the host around them,
// with utilization that follows the instances holding each card.
type Prober struct {
	spec  Spec
	cards *cards

	mu   sync.Mutex
	util []float64
}

// NewProber is the prober of spec's machine; cards is shared with its
// runtime, which records what each instance holds.
func NewProber(spec Spec, cards *cards) *Prober {
	return &Prober{spec: spec, cards: cards, util: make([]float64, spec.Count)}
}

func (p *Prober) Vendor() string  { return p.spec.Vendor }
func (p *Prober) Available() bool { return p.spec.Count > 0 }
func (p *Prober) Framework(context.Context) (string, string) {
	return p.spec.Framework, p.spec.FrameworkVersion
}
func (p *Prober) DriverVersion(context.Context) string { return p.spec.Driver }
func (p *Prober) Topology(context.Context) string      { return "" }

// RuntimeInjection is empty: the simulated runtime starts no containers.
func (p *Prober) RuntimeInjection([]int) gpu.RuntimeInjection { return gpu.RuntimeInjection{} }

// Synthetic, SimulatesHost and SimulatesPartitioning mark the node as
// simulated (gpu.HostSimulator, gpu.PartitionSimulator).
func (*Prober) Synthetic() bool     { return true }
func (*Prober) SimulatesHost() bool { return true }
func (p *Prober) SimulatesPartitioning() bool {
	return p.spec.Vendor == "nvidia"
}

func (p *Prober) Probe(context.Context) ([]proto.GPUInfo, error) {
	out := make([]proto.GPUInfo, p.spec.Count)
	for i := range out {
		archID := p.spec.ArchID
		if archID == "" {
			archID = p.spec.ComputeCapability
		}
		out[i] = proto.GPUInfo{
			Index: i, UUID: p.cardUUID(i), Name: p.spec.Card, MemoryTotalBytes: p.spec.VRAMMiB << 20,
			Vendor: p.spec.Vendor, ArchID: archID, ComputeCapability: p.spec.ComputeCapability, Healthy: true,
		}
	}
	return out, nil
}

func (p *Prober) cardUUID(i int) string { return fmt.Sprintf("GPU-SIM-%s-%d", p.spec.Hostname, i) }

// Metrics are what a vendor tool would report: a card an instance holds runs
// as busy as the hour's load (Rhythm), with its memory (or its dGPU slice's)
// mostly reserved by the engine; an idle card rests. A random walk toward the
// target keeps the curves moving without jumps.
func (p *Prober) Metrics(context.Context) ([]gpu.Metric, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := float64(p.spec.VRAMMiB << 20)
	busy := BusyUtilization(Rhythm(time.Now()))
	out := make([]gpu.Metric, p.spec.Count)
	for i := range out {
		share := p.cards.share(i)
		target := 0.5
		if share > 0 {
			// A card shared by slices runs as busy as its engines, scaled
			// by how much of it they hold.
			target = busy*math.Min(1, 0.4+share) + p.bias(i)
		}
		p.util[i] += (target-p.util[i])*0.3 + rand.NormFloat64()*4
		p.util[i] = math.Min(math.Max(p.util[i], 0), 100)
		used := total * 0.006
		if share > 0 {
			used = total * math.Min(0.97, share*(0.89+rand.Float64()*0.015)+0.004)
		}
		load := p.util[i] / 100
		out[i] = gpu.Metric{Index: i, UUID: p.cardUUID(i), Vendor: p.spec.Vendor, Values: map[string]float64{
			"gpuworks_gpu_utilization":         math.Round(p.util[i]),
			"gpuworks_gpu_memory_used_bytes":   used,
			"gpuworks_gpu_memory_total_bytes":  total,
			"gpuworks_gpu_power_watts":         math.Round(p.spec.TDP*(0.14+0.78*load) + rand.Float64()*8),
			"gpuworks_gpu_temperature_celsius": math.Round(33 + 42*load + rand.Float64()*2),
		}}
	}
	return out, nil
}

// bias sets a card a little apart from its neighbours, the way identical
// cards never read the same.
func (p *Prober) bias(i int) float64 { return float64((i*7+len(p.spec.Hostname))%9) - 4 }

// DescribeHost reports the simulated machine, never the real host
// (gpu.HostDescriber).
func (p *Prober) DescribeHost(sys proto.SysInfo) proto.SysInfo {
	const kibPerGiB = 1 << 20
	sys.Hostname, sys.OS, sys.Arch, sys.Kernel = p.spec.Hostname, p.spec.OS, p.spec.Arch, p.spec.Kernel
	sys.CPUModel, sys.CPUCores = p.spec.CPU, p.spec.Cores
	sys.MemTotalKB = p.spec.MemGiB * kibPerGiB
	sys.MemAvailKB = sys.MemTotalKB * (70 + int64(rand.IntN(5))) / 100
	sys.DiskTotalKB, sys.DiskAvailKB = 7153*kibPerGiB, 5820*kibPerGiB
	return sys
}

// MayDial lets a tunnel reach only the loopback ports the node's instances
// claimed from the engine host (gpu.PortConfiner).
func (p *Prober) MayDial(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && p.cards.claimed(n)
}

// cards records which cards the node's instances hold, and how much of
// each (a dGPU slice holds part of one), and the engine ports they claimed.
type cards struct {
	mu     sync.Mutex
	byID   map[string]holding
	shares map[int]float64
	ports  map[string]int // instance -> engine port
}

type holding struct {
	indexes []int
	share   float64
}

func newCards() *cards {
	return &cards{byID: map[string]holding{}, shares: map[int]float64{}, ports: map[string]int{}}
}

func (c *cards) claim(id string, port int) {
	c.mu.Lock()
	c.ports[id] = port
	c.mu.Unlock()
}

func (c *cards) claimed(port int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.ports {
		if p == port {
			return true
		}
	}
	return false
}

func (c *cards) hold(id string, indexes []int, share float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byID[id]; ok {
		return
	}
	c.byID[id] = holding{indexes: append([]int(nil), indexes...), share: share}
	for _, i := range indexes {
		c.shares[i] += share
	}
}

func (c *cards) release(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.byID[id]
	for _, i := range h.indexes {
		if c.shares[i] -= h.share; c.shares[i] <= 1e-9 {
			delete(c.shares, i)
		}
	}
	delete(c.byID, id)
	delete(c.ports, id)
}

// share is how much of card i the node's instances hold, at most all of it.
func (c *cards) share(i int) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return math.Min(1, c.shares[i])
}
