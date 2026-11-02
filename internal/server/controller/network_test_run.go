package controller

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nettest"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeCaller is the RPC half of the hub a network test needs. It is optional
// so controller harnesses that never test networks need not implement it.
type nodeCaller interface {
	CallNode(ctx context.Context, uuid, method string, params any) (json.RawMessage, error)
}

// onNetworkTestEvent starts a requested network test. The test probes every
// ordered pair of nodes and can take minutes, so it runs outside the
// deployment work queue.
func (c *Controller) onNetworkTestEvent(key, reason string) {
	if reason != "started" {
		return
	}
	id, err := strconv.ParseUint(key, 10, 64)
	if err != nil {
		return
	}
	go c.runNetworkTest(uint(id))
}

func (c *Controller) runNetworkTest(id uint) {
	run, err := c.store.NetworkTestRunByID(id)
	if err != nil || run.Status != store.NetworkTestRunning {
		return
	}
	caller, ok := c.hub.(nodeCaller)
	if !ok {
		c.log.Warn("network test cannot run: hub has no node RPC", "run", id)
		_ = c.store.AbandonNetworkTestRun(id)
		return
	}
	endpoints, err := c.networkTestEndpoints(run)
	if err != nil {
		c.log.Warn("network test cannot resolve its nodes", "run", id, "err", err)
		_ = c.store.AbandonNetworkTestRun(id)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	protocol := ""
	if tested, getErr := c.store.GetNetwork(run.NetworkID); getErr == nil && tested.Type == netplan.TypeAcceleratorHCCS {
		protocol = proto.AcceleratorPingProtocolHCCS
	}
	mode := nettest.ModeTCP
	if run.Kind == nettest.ModeRDMATool || nettest.CardMode(run.Kind) {
		mode = run.Kind
	}
	pairs := nettest.Run(ctx, caller.CallNode, endpoints, nettest.Options{Mode: mode, Pattern: run.Pattern, Protocol: protocol, Bandwidth: run.Bandwidth, Seconds: run.Seconds})
	passed := 0
	for _, p := range pairs {
		if nettest.PairStatus(mode, p, run.Bandwidth) == nettest.StatusPassed {
			passed++
		}
	}
	encoded, _ := json.Marshal(pairs)
	if err := c.store.FinishNetworkTestRun(id, string(encoded), len(pairs), passed); err != nil {
		c.log.Warn("network test result was not saved", "run", id, "err", err)
	}
}

// networkTestEndpoints resolves, at run time, each chosen node's address and
// rails on the tested network. A node that no longer resolves to it keeps an
// empty IP and fails its pairs with that reason. A ring runs in node-name
// order, so the pairs are the same from one run to the next.
func (c *Controller) networkTestEndpoints(run *store.NetworkTestRun) ([]nettest.Endpoint, error) {
	var uuids []string
	if err := json.Unmarshal([]byte(run.Nodes), &uuids); err != nil {
		return nil, err
	}
	nodes, err := c.store.ListNodes()
	if err != nil {
		return nil, err
	}
	tested, err := c.networks.Endpoints(run.NetworkID, nodes)
	if err != nil {
		return nil, err
	}
	resolved := map[uint]netplan.Resolved{}
	for _, r := range tested {
		if netplan.TestedRole(r.Role) {
			resolved[r.NodeID] = r
		}
	}
	byUUID := map[string]store.Node{}
	for _, n := range nodes {
		byUUID[n.UUID] = n
	}
	out := make([]nettest.Endpoint, 0, len(uuids))
	names := map[string]string{}
	for _, uuid := range uuids {
		ep := nettest.Endpoint{NodeUUID: uuid}
		node, ok := byUUID[uuid]
		names[uuid] = node.DisplayName()
		switch {
		case !ok:
			ep.Unavailable = nettest.CodeDeleted
		case !c.hub.Online(uuid):
			ep.Unavailable = nettest.CodeOffline
		default:
			r := resolved[node.ID]
			ep.IP = r.IP
			for _, rail := range netplan.Rails(network.Inventory(node), r.Network.Type, r.HCAs) {
				ep.Rails = append(ep.Rails, nettest.Rail{Name: rail.Name, Addr: rail.Addr})
			}
			if run.Kind == nettest.ModeRDMATool {
				ep.RDMAUnavailable = rdmaToolGap(node, ep.Rails)
			}
		}
		out = append(out, ep)
	}
	if run.Pattern == nettest.PatternRing {
		sort.SliceStable(out, func(i, j int) bool {
			if names[out[i].NodeUUID] != names[out[j].NodeUUID] {
				return names[out[i].NodeUUID] < names[out[j].NodeUUID]
			}
			return out[i].NodeUUID < out[j].NodeUUID
		})
	}
	return out, nil
}

// rdmaToolGap is the nettest code for why a node cannot run ib_write_bw, ""
// when it can.
func rdmaToolGap(node store.Node, rails []nettest.Rail) string {
	if len(rails) == 0 {
		return nettest.CodeNoHCA
	}
	if inv := network.Inventory(node); inv != nil {
		for _, tool := range inv.Tools {
			if tool == perftestTool {
				return ""
			}
		}
	}
	return nettest.CodeToolMissing
}

// perftestTool is the tool name agents report in NetInventory.Tools.
const perftestTool = "ib_write_bw"
