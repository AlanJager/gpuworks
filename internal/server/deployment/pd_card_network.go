package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nettest"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Card probes are observations, not a permanent fabric contract. After a day
// they cannot verify a new placement; the operator must test again.
const pdCardTestMaxAge = 24 * time.Hour

const pdCardAlternativesLimit = 8

func (s *Service) checkPDCardNetwork(p *pdRequest, group netplan.Group) ([]messages.Warning, error) {
	if p.mode.Transport != pdmode.TransportHCCL || !netplan.IsAccelerator(group.Network.Type) || len(p.nodes) < 2 {
		return nil, nil
	}
	run, err := s.d.Store.LatestNetworkCardTestRun(group.Network.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return pdCardNetworkWarnings(p, group, run, time.Now()), nil
}

type pdCardPort struct {
	node *store.Node
	rail netplan.Rail
}

func (p pdCardPort) label() string {
	return fmt.Sprintf("%s/%s (%s)", p.node.DisplayName(), p.rail.Name, p.rail.IP())
}

func pdCardNetworkWarnings(p *pdRequest, group netplan.Group, run *store.NetworkTestRun, now time.Time) []messages.Warning {
	nodes := map[string]*store.Node{}
	for _, node := range p.nodes {
		nodes[node.UUID] = node
	}
	var pairs []nettest.Pair
	if run != nil && nettest.CardMode(run.Kind) && run.Status == store.NetworkTestDone && run.FinishedAt != nil &&
		!run.StartedAt.After(now) && now.Sub(run.StartedAt) <= pdCardTestMaxAge {
		if json.Unmarshal([]byte(run.Pairs), &pairs) != nil {
			pairs = nil
		}
	}
	probes := pdCardProbes(pairs)
	ports := func(role string) ([]pdCardPort, bool) {
		var out []pdCardPort
		unpinned := false
		for _, unit := range p.pd.Units(role) {
			for _, rank := range unit.Ranks() {
				node := nodes[rank.NodeUUID]
				if node == nil {
					continue
				}
				unpinned = unpinned || len(rank.GPUIndexes) == 0
				for _, index := range rank.GPUIndexes {
					name := accelerator.PortRailName(node.GPUVendor, index)
					rail := netplan.Rails(network.Inventory(*node), group.Network.Type, []string{name})[0]
					if !slices.Contains(group.Ranks[node.ID].HCAs, name) {
						rail = netplan.Rail{Name: name}
					}
					out = append(out, pdCardPort{node: node, rail: rail})
				}
			}
		}
		return out, unpinned
	}
	prefill, unpinnedP := ports(pdmode.RolePrefill)
	decode, unpinnedD := ports(pdmode.RoleDecode)
	var failed, unknown []string
	for _, source := range prefill {
		for _, target := range decode {
			if source.node.UUID == target.node.UUID {
				continue
			}
			label := source.label() + " ↔ " + target.label()
			probe := pdCardProbe(probes, source, target)
			switch {
			case probe == -1:
				failed = append(failed, label)
			case probe != 1:
				unknown = append(unknown, label)
			}
		}
	}
	available := pdAvailableCardPairs(probes, p.nodes, group)
	warning := func(code string, links []string) messages.Warning {
		return messages.NewWarning(code, map[string]any{
			"network": group.Network.Name, "pairs": strings.Join(links, "; "), "available": available,
			"network_id": group.Network.ID,
		})
	}
	var out []messages.Warning
	if len(failed) > 0 {
		out = append(out, warning(messages.WarningPDCardNetworkUnreachable, failed))
	}
	if len(unknown) > 0 || unpinnedP || unpinnedD {
		out = append(out, warning(messages.WarningPDCardNetworkUntested, unknown))
	}
	return out
}

// An UP port or matching card numbers does not prove peer reachability.
// Match both measured endpoint IPs; tool errors remain unknown.
func pdCardProbe(probes map[pdCardProbeKey]int, source, target pdCardPort) int {
	if source.rail.Gap() != "" || target.rail.Gap() != "" {
		return 0
	}
	return probes[pdCardProbeKey{source.node.UUID, source.rail.Name, source.rail.IP(), target.node.UUID, target.rail.Name, target.rail.IP()}]
}

type pdCardProbeKey struct {
	source, sourceRail, sourceIP string
	target, targetRail, targetIP string
}

func pdCardProbes(pairs []nettest.Pair) map[pdCardProbeKey]int {
	probes := map[pdCardProbeKey]int{}
	for _, pair := range pairs {
		for _, rail := range pair.Rails {
			status := 0
			if rail.Code == nettest.CodeUnreachable {
				status = -1
			} else if rail.OK() {
				status = 1
			}
			probes[pdCardProbeKey{pair.SourceUUID, rail.Rail, rail.SourceIP, pair.TargetUUID, rail.TargetRail, rail.TargetIP}] = status
			probes[pdCardProbeKey{pair.TargetUUID, rail.TargetRail, rail.TargetIP, pair.SourceUUID, rail.Rail, rail.SourceIP}] = status
		}
	}
	return probes
}

func pdAvailableCardPairs(probes map[pdCardProbeKey]int, nodes []*store.Node, group netplan.Group) string {
	var ports []pdCardPort
	for _, node := range nodes {
		for _, rail := range netplan.Rails(network.Inventory(*node), group.Network.Type, group.Ranks[node.ID].HCAs) {
			ports = append(ports, pdCardPort{node: node, rail: rail})
		}
	}
	var available []string
	for i, source := range ports {
		for _, target := range ports[i+1:] {
			if source.node.UUID != target.node.UUID && pdCardProbe(probes, source, target) == 1 {
				available = append(available, source.label()+" ↔ "+target.label())
				if len(available) == pdCardAlternativesLimit {
					return strings.Join(available, "; ")
				}
			}
		}
	}
	if len(available) == 0 {
		return "—"
	}
	slices.Sort(available)
	return strings.Join(slices.Compact(available), "; ")
}
