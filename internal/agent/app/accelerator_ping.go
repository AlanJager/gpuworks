package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Bounds on one accelerator.ping call. A probe that gets no answer costs its
// whole time limit, so the limit and the number of probes are both capped.
const (
	acceleratorPingDefaultTimeout = 3 * time.Second
	acceleratorPingMaxTimeout     = 10 * time.Second
	acceleratorPingMaxProbes      = 256
)

// rpcAcceleratorPing pings each probe's target from the port of its card.
// Probes are grouped by card: cards run side by side, the probes of one card
// in request order, and the answers come back in request order.
func (n *NodeRuntime) rpcAcceleratorPing(ctx context.Context, params json.RawMessage) (any, error) {
	var p proto.AcceleratorPingParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("accelerator ping params: %w", err)
	}
	if len(p.Probes) == 0 || len(p.Probes) > acceleratorPingMaxProbes {
		return nil, fmt.Errorf("accelerator ping takes 1 to %d probes", acceleratorPingMaxProbes)
	}
	timeout := time.Duration(p.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = acceleratorPingDefaultTimeout
	}
	if timeout > acceleratorPingMaxTimeout {
		timeout = acceleratorPingMaxTimeout
	}
	if p.Protocol == "" {
		p.Protocol = proto.AcceleratorPingProtocolRoCE
	}
	if p.Protocol != proto.AcceleratorPingProtocolRoCE && p.Protocol != proto.AcceleratorPingProtocolHCCS {
		return nil, fmt.Errorf("accelerator ping protocol %q is unsupported", p.Protocol)
	}
	results := make([]proto.AcceleratorPingResult, len(p.Probes))
	for i, probe := range p.Probes {
		results[i] = proto.AcceleratorPingResult{Card: probe.Card, Target: probe.Target}
	}
	pinger, ok := n.prober.(gpu.AcceleratorPortPinger)
	if !ok {
		for i := range results {
			results[i].Code, results[i].Error = proto.AcceleratorPingNoCard, "this node's accelerators have no network ports of their own"
		}
		return results, nil
	}
	byCard := map[string][]int{}
	var order []string
	for i, probe := range p.Probes {
		if _, seen := byCard[probe.Card]; !seen {
			order = append(order, probe.Card)
		}
		byCard[probe.Card] = append(byCard[probe.Card], i)
	}
	var wg sync.WaitGroup
	for _, card := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, i := range byCard[card] {
				var rtt float64
				var err error
				if p.Protocol == proto.AcceleratorPingProtocolHCCS {
					protocolPinger, supported := n.prober.(gpu.AcceleratorPortProtocolPinger)
					if !supported {
						err = errors.New("the accelerator prober does not support HCCS ping")
					} else {
						rtt, err = protocolPinger.PingAcceleratorPortProtocol(ctx, card, p.Probes[i].Target, p.Protocol, timeout)
					}
				} else {
					rtt, err = pinger.PingAcceleratorPort(ctx, card, p.Probes[i].Target, timeout)
				}
				switch {
				case err == nil:
					results[i].Reachable, results[i].RTTMS = true, rtt
				case errors.Is(err, gpu.ErrAcceleratorNoCard):
					results[i].Code, results[i].Error = proto.AcceleratorPingNoCard, "the node reports no card named "+card
				case errors.Is(err, gpu.ErrAcceleratorPingTimeout):
					results[i].Code, results[i].Error = proto.AcceleratorPingUnreachable, err.Error()
				default:
					results[i].Code, results[i].Error = proto.AcceleratorPingToolFailed, err.Error()
				}
			}
		}()
	}
	wg.Wait()
	return results, nil
}
