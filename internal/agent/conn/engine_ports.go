package conn

import (
	"context"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/netinfo"
	"gpuworks.ai/gpuworks/internal/proto"
)

// setEnginePorts records the engine port range a state sync carries. The
// first status of a connection goes out before the sync arrives, so a changed
// range reports again at once instead of waiting a status interval.
func (c *Conn) setEnginePorts(ctx context.Context, r *proto.PortRange) {
	if old := c.enginePorts.Swap(r); !samePortRange(old, r) {
		c.requestStatus(ctx)
	}
}

func samePortRange(a, b *proto.PortRange) bool {
	return a == b || (a != nil && b != nil && *a == *b)
}

// enginePortListeners lists the host's listeners inside the engine port range;
// nil until a server supplies one.
func (c *Conn) enginePortListeners() []int {
	r := c.enginePorts.Load()
	if r == nil {
		return nil
	}
	if l, ok := c.prober.(gpu.ListenerReporter); ok {
		return l.EnginePortListeners(*r)
	}
	ports, err := netinfo.ListenPorts(*r)
	if err != nil {
		c.log.Debug("engine port listeners incomplete", "err", err)
	}
	return ports
}
