package netplan

import (
	"net"
	"path"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

type candidate struct {
	network    Network
	attachment Attachment
	rank       int // 0 = node, 1 = label
}

// Resolve returns, for every node and role, the one network in effect. Node
// attachments outrank label attachments; within a rank the earliest attachment
// wins and the winner carries a conflict warning naming the losers. A role no
// attachment reaches falls to the management network unless the node turned
// that role off there or has not reported its management address.
func Resolve(in Inputs) Resolution {
	networks := make(map[uint]Network, len(in.Networks))
	for _, n := range in.Networks {
		networks[n.ID] = n
	}
	var out Resolution
	for _, node := range in.Nodes {
		for _, role := range Roles {
			candidates := roleCandidates(node, role, networks, in.Attachments)
			if len(candidates) == 0 {
				if mgmt, ok := managementFor(node, role); ok {
					out = append(out, mgmt)
				}
				continue
			}
			out = append(out, resolveWinner(node, role, candidates))
		}
	}
	flagLinkLayerMismatch(out)
	flagRailMismatch(out)
	return out
}

func resolveWinner(node Node, role string, candidates []candidate) Resolved {
	winner := candidates[0]
	resolved := Resolved{
		NodeID:       node.ID,
		Role:         role,
		Network:      winner.network,
		AttachmentID: winner.attachment.ID,
		Source:       SourceNode,
	}
	if winner.rank == 1 {
		resolved.Source = SourceLabel
		resolved.LabelKey, resolved.LabelValue = winner.attachment.LabelKey, winner.attachment.LabelValue
	}
	var losers []string
	for _, c := range candidates[1:] {
		if c.rank == winner.rank {
			losers = append(losers, c.network.Name)
		}
	}
	if len(losers) > 0 {
		resolved.Warnings = append(resolved.Warnings, Warning{
			Code:   WarningConflict,
			Params: map[string]string{"role": role, "winner": winner.network.Name, "losers": strings.Join(losers, ", ")},
		})
	}
	selectEndpoints(&resolved, winner, node)
	return resolved
}

// roleCandidates returns the networks reaching a node for one role, best
// first. A network attached both directly and by label counts once, at its
// best rank.
func roleCandidates(node Node, role string, networks map[uint]Network, attachments []Attachment) []candidate {
	var byRank [2][]candidate
	seen := map[uint]bool{}
	for pass := 0; pass < 2; pass++ {
		for _, a := range attachments {
			network, ok := networks[a.NetworkID]
			if !ok || !network.HasRole(role) {
				continue
			}
			matches := (pass == 0 && a.NodeID == node.ID) ||
				(pass == 1 && a.NodeID == 0 && a.LabelKey != "" && node.Labels[a.LabelKey] == a.LabelValue)
			if !matches || seen[network.ID] {
				continue
			}
			seen[network.ID] = true
			byRank[pass] = append(byRank[pass], candidate{network: network, attachment: a, rank: pass})
		}
	}
	return append(byRank[0], byRank[1]...)
}

func selectEndpoints(r *Resolved, c candidate, node Node) {
	if UsesRails(c.network.Type) {
		selectRDMA(r, node, c)
		return
	}
	a := c.attachment
	overrideIface, overrideIP := strings.TrimSpace(a.Interface), strings.TrimSpace(a.IP)
	if overrideIface != "" || overrideIP != "" {
		selectOverride(r, node.Inventory, overrideIface, overrideIP)
	} else if node.Inventory != nil {
		r.Interface, r.IP = matchInterface(node.Inventory, c.network)
	}
}

// selectOverride takes a node attachment's pinned interface or IP, completed
// from the inventory, and warns when the inventory lacks it.
func selectOverride(r *Resolved, inv *proto.NetInventory, overrideIface, overrideIP string) {
	r.Interface, r.IP = overrideIface, overrideIP
	if inv == nil {
		return
	}
	if iface, ip, ok := findOverride(inv, overrideIface, overrideIP); ok {
		r.Interface, r.IP = iface, ip
		return
	}
	value := overrideIface
	if overrideIP != "" {
		value = strings.TrimSpace(overrideIface + " " + overrideIP)
	}
	r.Warnings = append(r.Warnings, Warning{Code: WarningOverrideMissing, Params: map[string]string{"kind": "interface", "value": value}})
}

// matchInterface picks the first interface (inventory order is by name) whose
// name matches the glob and that holds an address in the CIDR. Without a CIDR
// the interface's first IPv4 address, else its first address, is used.
func matchInterface(inv *proto.NetInventory, n Network) (string, string) {
	glob := strings.TrimSpace(n.Interface)
	var subnet *net.IPNet
	if cidr := strings.TrimSpace(n.CIDR); cidr != "" {
		if _, parsed, err := net.ParseCIDR(cidr); err == nil {
			subnet = parsed
		}
	}
	if glob == "" && subnet == nil {
		return "", ""
	}
	for _, iface := range inv.Interfaces {
		if glob != "" {
			if ok, _ := path.Match(glob, iface.Name); !ok {
				continue
			}
		}
		if ip := pickAddress(iface.Addrs, subnet); ip != "" || (subnet == nil && glob != "") {
			return iface.Name, ip
		}
	}
	return "", ""
}

func pickAddress(addrs []string, subnet *net.IPNet) string {
	fallback := ""
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr)
		if err != nil {
			continue
		}
		if subnet != nil {
			if subnet.Contains(ip) {
				return ip.String()
			}
			continue
		}
		if ip.To4() != nil {
			return ip.String()
		}
		if fallback == "" {
			fallback = ip.String()
		}
	}
	return fallback
}

func findOverride(inv *proto.NetInventory, iface, ip string) (string, string, bool) {
	for _, candidate := range inv.Interfaces {
		if iface != "" && candidate.Name != iface {
			continue
		}
		if ip == "" {
			return candidate.Name, pickAddress(candidate.Addrs, nil), true
		}
		for _, addr := range candidate.Addrs {
			if parsed, _, err := net.ParseCIDR(addr); err == nil && parsed.String() == ip {
				return candidate.Name, ip, true
			}
		}
	}
	return "", "", false
}

// findHCA accepts "mlx5_0" (its first port) or "mlx5_0:1" on a host RDMA
// network, and a card's name ("npu3") on an accelerator one.
func findHCA(inv *proto.NetInventory, networkType, hca string) (string, bool) {
	if networkType == TypeAcceleratorRDMA {
		for _, p := range inv.AcceleratorPorts {
			if p.Name == hca {
				return p.Name, true
			}
		}
		return "", false
	}
	device, _, hasPort := strings.Cut(hca, ":")
	for _, p := range inv.RDMAPorts {
		if p.Device != device {
			continue
		}
		if name := hcaName(p); !hasPort || name == hca {
			return name, true
		}
	}
	return "", false
}

// portsLinkLayer names the link layer of the selected ports, "" when unknown
// or when the selection mixes both kinds. Accelerator ports are RoCE.
func portsLinkLayer(inv *proto.NetInventory, hcas []string) string {
	if inv == nil {
		return ""
	}
	layers := map[string]bool{}
	for _, hca := range hcas {
		for _, p := range inv.AcceleratorPorts {
			if p.Name == hca {
				layers[LinkLayerRoCE] = true
			}
		}
		for _, p := range inv.RDMAPorts {
			if hcaName(p) == hca {
				if strings.EqualFold(p.LinkLayer, "InfiniBand") {
					layers[LinkLayerInfiniBand] = true
				} else {
					layers[LinkLayerRoCE] = true
				}
			}
		}
	}
	if len(layers) != 1 {
		return ""
	}
	for layer := range layers {
		return layer
	}
	return ""
}

// flagLinkLayerMismatch warns on every node of an RDMA network whose nodes
// resolved to more than one link layer.
func flagLinkLayerMismatch(out Resolution) {
	layers := map[uint]map[string]bool{}
	for _, r := range out {
		if r.LinkLayer == "" {
			continue
		}
		if layers[r.Network.ID] == nil {
			layers[r.Network.ID] = map[string]bool{}
		}
		layers[r.Network.ID][r.LinkLayer] = true
	}
	for i := range out {
		seen := layers[out[i].Network.ID]
		if len(seen) < 2 {
			continue
		}
		names := make([]string, 0, len(seen))
		for layer := range seen {
			names = append(names, layer)
		}
		sort.Strings(names)
		out[i].Warnings = append(out[i].Warnings, Warning{
			Code: WarningLinkLayerMismatch, Params: map[string]string{"link_layers": strings.Join(names, ", ")},
		})
	}
}

func hcaName(p proto.RDMAPort) string {
	return p.Device + ":" + strconv.Itoa(p.Port)
}
