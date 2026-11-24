package deployment

import (
	"encoding/json"
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
	now := time.Now().UTC()
	runs, err := s.d.Store.RecentNetworkCardTestRuns(group.Network.ID, now.Add(-pdCardTestMaxAge))
	if err != nil {
		return nil, err
	}
	warnings := pdCardNetworkWarnings(p, group, runs, now)
	if p.singlePinnedPDCardPair() {
		for _, warning := range warnings {
			if warning.Code == messages.WarningPDCardNetworkUnreachable {
				return nil, productError(messages.ErrorPDCardNetworkBlocked, fmt.Sprintf(
					"the selected PD ports on %s cannot reach each other: %s (verified pairs: %s); select a reachable pair or run the card matrix test again",
					warning.Params["network"], warning.Params["pairs"], warning.Params["available"],
				), warning.Params)
			}
		}
	}
	return warnings, nil
}

// Only a single pinned card per role proves which link the engine must use.
func (p *pdRequest) singlePinnedPDCardPair() bool {
	for _, role := range []string{pdmode.RolePrefill, pdmode.RoleDecode} {
		units := p.pd.Units(role)
		if len(units) != 1 || len(units[0].Followers) != 0 || len(units[0].GPUIndexes) != 1 {
			return false
		}
	}
	return true
}

type pdCardPort struct {
	node *store.Node
	rail netplan.Rail
}

func (p pdCardPort) label() string {
	return fmt.Sprintf("%s/%s (%s)", p.node.DisplayName(), p.rail.Name, p.rail.IP())
}

func pdCardNetworkWarnings(p *pdRequest, group netplan.Group, runs []store.NetworkTestRun, now time.Time) []messages.Warning {
	nodes := map[string]*store.Node{}
	for _, node := range p.nodes {
		nodes[node.UUID] = node
	}
	probes := pdCardProbes(runs, nodes, now)
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

// Each pair keeps its newest conclusive result within the validity window.
func pdCardProbes(runs []store.NetworkTestRun, nodes map[string]*store.Node, now time.Time) map[pdCardProbeKey]int {
	probes := map[pdCardProbeKey]int{}
	for _, run := range runs {
		if !nettest.CardMode(run.Kind) || run.Status != store.NetworkTestDone || run.FinishedAt == nil ||
			run.StartedAt.After(now) || now.Sub(run.StartedAt) > pdCardTestMaxAge {
			continue
		}
		var pairs []nettest.Pair
		if json.Unmarshal([]byte(run.Pairs), &pairs) != nil {
			continue
		}
		for _, pair := range pairs {
			if nodes[pair.SourceUUID] == nil || nodes[pair.TargetUUID] == nil {
				continue
			}
			for _, rail := range pair.Rails {
				key := pdCardProbeKey{pair.SourceUUID, rail.Rail, rail.SourceIP, pair.TargetUUID, rail.TargetRail, rail.TargetIP}
				if probes[key] != 0 {
					continue
				}
				var status int
				switch {
				case rail.Code == nettest.CodeUnreachable:
					status = -1
				case rail.OK():
					status = 1
				default:
					continue
				}
				probes[key] = status
				probes[pdCardProbeKey{pair.TargetUUID, rail.TargetRail, rail.TargetIP, pair.SourceUUID, rail.Rail, rail.SourceIP}] = status
			}
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
