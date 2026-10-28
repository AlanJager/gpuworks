package gpu

import "gpuworks.ai/gpuworks/internal/proto"

// A simulated node (the experience environment's) stands for a machine that
// is not the host its agent runs on. Its prober says so through the optional
// interfaces below; a real prober implements none of them, so a real node is
// unaffected.

// HostSimulator is a prober standing for another machine: the agent then
// refuses whatever would reach the real host (a shell, files outside the
// node's directory, its own binary, the host's logs).
type HostSimulator interface {
	SimulatesHost() bool
}

// SimulatesHost reports whether p stands for another machine.
func SimulatesHost(p Prober) bool {
	s, ok := p.(HostSimulator)
	return ok && s.SimulatesHost()
}

// HostDescriber is a prober that also describes the machine it stands for,
// so a simulated node reports that machine and not the real host.
type HostDescriber interface {
	DescribeHost(proto.SysInfo) proto.SysInfo
}

// HostInfo returns sys as p's machine reports it; for any other prober, sys
// unchanged.
func HostInfo(p Prober, sys proto.SysInfo) proto.SysInfo {
	if d, ok := p.(HostDescriber); ok {
		return d.DescribeHost(sys)
	}
	return sys
}

// PartitionSimulator is a prober whose cards offer dGPU slicing without the
// native library: its runtime never loads it.
type PartitionSimulator interface {
	SimulatesPartitioning() bool
}

// SimulatesPartitioning reports whether p offers simulated dGPU slicing.
func SimulatesPartitioning(p Prober) bool {
	s, ok := p.(PartitionSimulator)
	return ok && s.SimulatesPartitioning()
}

// PortConfiner is a simulated node's prober that names the local ports the
// node may reach: those its own instances' engines answer on. A tunnel to
// any other would reach a service of the real host.
type PortConfiner interface {
	MayDial(addr string) bool
}

// MayDial reports whether a node with prober p may open a tunnel to addr: a
// real node may; a simulated one only to its engines.
func MayDial(p Prober, addr string) bool {
	if c, ok := p.(PortConfiner); ok {
		return c.MayDial(addr)
	}
	return !SimulatesHost(p)
}

// HostSampler is a simulated node's prober that reports the machine's CPU,
// disk and network figures, in place of the real host's.
type HostSampler interface {
	HostSamples(node string, now int64) []proto.MetricSample
}

// ListenerReporter is a simulated node's prober that names the listeners in
// the engine port range its host has: the ports the other simulated nodes'
// instances hold on the one machine they share.
type ListenerReporter interface {
	EnginePortListeners(proto.PortRange) []int
}
