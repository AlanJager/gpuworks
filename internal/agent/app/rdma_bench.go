package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/perftest"
	"gpuworks.ai/gpuworks/internal/proto"
)

// rdma.bench.* run ib_write_bw rail by rail: the target node serves on every
// rail, then the source connects rail i to the target's port BasePort+i. See
// internal/server/nettest for the orchestration.

func parseRDMABench(params json.RawMessage) (proto.RDMABenchParams, []perftest.Rail, error) {
	var p proto.RDMABenchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, nil, err
	}
	if len(p.Rails) == 0 || len(p.Rails) > 32 {
		return p, nil, fmt.Errorf("rdma bench needs 1 to 32 rails")
	}
	if p.BasePort < 1024 || p.BasePort+len(p.Rails) > 65535 {
		return p, nil, fmt.Errorf("invalid rdma bench base port %d", p.BasePort)
	}
	if p.Seconds < 1 || p.Seconds > 30 {
		return p, nil, fmt.Errorf("rdma bench duration must be between 1 and 30 seconds")
	}
	rails := make([]perftest.Rail, 0, len(p.Rails))
	for _, raw := range p.Rails {
		rail, err := perftest.ParseRail(raw)
		if err != nil {
			return p, nil, err
		}
		rails = append(rails, rail)
	}
	return p, rails, nil
}

func (n *NodeRuntime) rpcRDMABenchServe(params json.RawMessage) (any, error) {
	p, rails, err := parseRDMABench(params)
	if err != nil {
		return nil, err
	}
	// The servers outlive this RPC: they wait for the peer's clients and
	// exit after one test, or when this bound expires.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.Seconds+60)*time.Second)
	failed := n.perftest.Serve(ctx, rails, p.BasePort, p.Seconds)
	if len(failed) == len(rails) {
		cancel()
	} else {
		go func() {
			<-ctx.Done()
			cancel()
		}()
	}
	return proto.RDMABenchServed{Failed: failed}, nil
}

func (n *NodeRuntime) rpcRDMABenchRun(ctx context.Context, params json.RawMessage) (any, error) {
	p, rails, err := parseRDMABench(params)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(p.Target) == nil {
		return nil, fmt.Errorf("rdma bench target must be an IP address")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.Seconds+30)*time.Second)
	defer cancel()
	return n.perftest.Run(ctx, p.Target, rails, p.BasePort, p.Seconds), nil
}

// NetworkTools names the network test tools this node can run, for the
// network inventory: ib_write_bw counts when the host's or the embedded one
// starts.
func (n *NodeRuntime) NetworkTools() []string {
	if n.perftest.Locate != nil && n.perftest.Available() {
		return []string{perftest.Tool}
	}
	return nil
}
