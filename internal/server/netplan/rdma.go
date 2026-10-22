package netplan

import (
	"net"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A network that uses rails asks the operator for nothing: the rails and the
// bootstrap address (the IP the socket handshake of NCCL, HCCL, Gloo and
// ib_write_bw uses) are inferred from the node's reported inventory. The
// operator may still pin a node's rails or bootstrap on its attachment, and
// may give the network a bootstrap CIDR. See docs/how-to/configure-networks.md.
//
// A host RDMA network's rails are the host's RDMA ports. An accelerator RDMA
// network's rails are the accelerator cards, each through the RoCE port it
// owns; the host cannot bind those addresses, so its bootstrap is always a
// host address of its own.

// Rail is one RDMA port, or one accelerator card's port, a node uses on a
// network.
type Rail struct {
	Name      string // "device:port", or the card ("npu3")
	NetDev    string // the host interface on the same PCI function; "" for IB without IPoIB and for a card
	Addr      string // the rail's first IPv4 address with its prefix; "" when it has none
	LinkLayer string // LinkLayerInfiniBand | LinkLayerRoCE; "" when the port is not in the inventory
	State     string // as reported: ACTIVE, DOWN, ... for an RDMA port; UP or DOWN for a card's port
	SpeedGbps float64
}

// IP is the rail's IPv4 address without its prefix.
func (r Rail) IP() string {
	ip, _, _ := strings.Cut(r.Addr, "/")
	return ip
}

// Why a rail cannot carry RDMA traffic.
const (
	RailInactive = "inactive" // the port is not ACTIVE
	RailNoIPv4   = "no_ipv4"  // a RoCE port whose interface has no IPv4 address, so no RoCE v2 GID
	RailUnknown  = "unknown"  // the port is not in the node's inventory
)

// linkUp reads the rail's link state in its own vocabulary: an RDMA port is
// ACTIVE, a card's port is UP.
func (r Rail) linkUp() bool {
	return strings.EqualFold(r.State, "ACTIVE") || strings.EqualFold(r.State, "UP")
}

// Gap says why the rail cannot carry RDMA traffic, "" when it can.
func (r Rail) Gap() string {
	switch {
	case r.LinkLayer == "":
		return RailUnknown
	case !r.linkUp():
		return RailInactive
	case r.LinkLayer == LinkLayerRoCE && r.Addr == "":
		return RailNoIPv4
	}
	return ""
}

// Rails describes the named rails of a network of the given type from the
// inventory, in the given order. A name the inventory lacks keeps only its
// name.
func Rails(inv *proto.NetInventory, networkType string, names []string) []Rail {
	reported := AllRails(inv, networkType)
	out := make([]Rail, 0, len(names))
	for _, name := range names {
		rail := Rail{Name: name}
		for _, r := range reported {
			if r.Name == name {
				rail = r
				break
			}
		}
		out = append(out, rail)
	}
	return out
}

// AllRails describes every rail the node reported for a network of the given
// type, in inventory order: the choices for adding a rail by hand.
func AllRails(inv *proto.NetInventory, networkType string) []Rail {
	if inv == nil {
		return []Rail{}
	}
	if networkType == TypeAcceleratorRDMA {
		out := make([]Rail, 0, len(inv.AcceleratorPorts))
		for _, p := range inv.AcceleratorPorts {
			out = append(out, describeAcceleratorRail(p))
		}
		return out
	}
	out := make([]Rail, 0, len(inv.RDMAPorts))
	for _, p := range inv.RDMAPorts {
		out = append(out, describeRail(inv, p))
	}
	return out
}

// describeAcceleratorRail is a card's port as a rail.
func describeAcceleratorRail(p proto.AcceleratorPort) Rail {
	rail := Rail{Name: p.Name, LinkLayer: LinkLayerRoCE, State: p.State, SpeedGbps: p.SpeedGbps}
	if ip := net.ParseIP(p.IP); ip != nil && ip.To4() != nil {
		rail.Addr = p.IP
		if mask := net.ParseIP(p.Netmask); mask != nil && mask.To4() != nil {
			if ones, bits := net.IPMask(mask.To4()).Size(); bits == 32 {
				rail.Addr = p.IP + "/" + strconv.Itoa(ones)
			}
		}
	}
	return rail
}

func describeRail(inv *proto.NetInventory, p proto.RDMAPort) Rail {
	rail := Rail{Name: hcaName(p), NetDev: p.NetDev, State: p.State, SpeedGbps: p.RateGbps, LinkLayer: LinkLayerRoCE}
	if strings.EqualFold(p.LinkLayer, "InfiniBand") {
		rail.LinkLayer = LinkLayerInfiniBand
	}
	if iface, ok := findInterface(inv, p.NetDev); ok {
		rail.Addr = firstIPv4(iface.Addrs)
		// A RoCE port's own rate is often wrong (every port of an hns NIC
		// reports the same one); the interface's link speed is not.
		if rail.LinkLayer == LinkLayerRoCE && iface.SpeedMbps > 0 {
			rail.SpeedGbps = float64(iface.SpeedMbps) / 1000
		}
	}
	return rail
}

// inferRails picks the rails of a node nobody pinned. On a host RDMA network:
// its usable InfiniBand ports, or on a host without them its usable RoCE
// ports, keeping only the fastest of them. A GPU server's compute rails are
// one kind and one speed; slower ports are management or storage NICs. On an
// accelerator RDMA network: every card whose port is usable, in card order.
func inferRails(inv *proto.NetInventory, networkType string) []string {
	if networkType == TypeAcceleratorRDMA {
		out := []string{}
		for _, rail := range AllRails(inv, networkType) {
			if rail.Gap() == "" {
				out = append(out, rail.Name)
			}
		}
		return out
	}
	var ib, roce []Rail
	for _, rail := range AllRails(inv, networkType) {
		if rail.Gap() != "" {
			continue
		}
		if rail.LinkLayer == LinkLayerInfiniBand {
			ib = append(ib, rail)
		} else {
			roce = append(roce, rail)
		}
	}
	rails := roce
	if len(ib) > 0 {
		rails = ib
	}
	fastest := 0.0
	for _, r := range rails {
		fastest = max(fastest, r.SpeedGbps)
	}
	out := []string{}
	for _, r := range rails {
		if r.SpeedGbps == fastest {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

// selectRDMA fills the rails and bootstrap of a network that uses rails on
// one node.
func selectRDMA(r *Resolved, node Node, c candidate) {
	inv, a := node.Inventory, c.attachment
	if len(a.HCAs) > 0 {
		r.HCAsPinned = true
		r.HCAs = append([]string(nil), a.HCAs...)
		if inv != nil {
			var missing, unusable []string
			for i, hca := range r.HCAs {
				port, ok := findHCA(inv, c.network.Type, hca)
				if !ok {
					missing = append(missing, hca)
					continue
				}
				r.HCAs[i] = port
				if Rails(inv, c.network.Type, []string{port})[0].Gap() != "" {
					unusable = append(unusable, port)
				}
			}
			if len(missing) > 0 {
				r.Warnings = append(r.Warnings, Warning{Code: WarningOverrideMissing, Params: map[string]string{"kind": "hca", "value": strings.Join(missing, ", ")}})
			}
			if len(unusable) > 0 {
				r.Warnings = append(r.Warnings, Warning{Code: WarningHCAUnusable, Params: map[string]string{"value": strings.Join(unusable, ", ")}})
			}
		}
	} else if inv != nil {
		r.HCAs = inferRails(inv, c.network.Type)
	}
	r.LinkLayer = portsLinkLayer(inv, r.HCAs)

	overrideIface, overrideIP := strings.TrimSpace(a.Interface), strings.TrimSpace(a.IP)
	switch {
	case overrideIface != "" || overrideIP != "":
		r.BootstrapPinned = true
		selectOverride(r, inv, overrideIface, overrideIP)
	case inv == nil:
	case strings.TrimSpace(c.network.CIDR) != "":
		r.Interface, r.IP = matchInterface(inv, Network{CIDR: c.network.CIDR})
	case c.network.Type == TypeAcceleratorRDMA:
		// No host process can bind a card's address, so the handshake rides
		// the one host address every node has.
		if addr, ok := node.Management(); ok {
			r.Interface, r.IP = addr.Interface, addr.IP
		}
	default:
		r.Interface, r.IP = inferBootstrap(node, Rails(inv, c.network.Type, r.HCAs))
	}
	if inv == nil {
		return
	}
	if len(r.HCAs) == 0 {
		r.Warnings = append(r.Warnings, Warning{Code: WarningNoHCA})
	}
	if r.IP == "" {
		r.Warnings = append(r.Warnings, Warning{Code: WarningBootstrapMissing})
	}
}

// inferBootstrap is the first rail's own address (RoCE, or InfiniBand with
// IPoIB), so the handshake and other socket traffic of the job stays on the
// fast network; InfiniBand without IPoIB falls back to the management
// address, which every node has.
func inferBootstrap(node Node, rails []Rail) (string, string) {
	if len(rails) > 0 && rails[0].Addr != "" {
		return rails[0].NetDev, rails[0].IP()
	}
	if len(rails) > 0 && rails[0].LinkLayer == LinkLayerInfiniBand {
		if addr, ok := node.Management(); ok {
			return addr.Interface, addr.IP
		}
	}
	return "", ""
}

// flagRailMismatch warns on every node of a network that uses rails whose
// nodes use different numbers of them: rails pair up across nodes, so an
// extra rail on one node has no peer.
func flagRailMismatch(out Resolution) {
	counts := map[uint]map[int]bool{}
	for _, r := range out {
		if !UsesRails(r.Network.Type) || len(r.HCAs) == 0 {
			continue
		}
		if counts[r.Network.ID] == nil {
			counts[r.Network.ID] = map[int]bool{}
		}
		counts[r.Network.ID][len(r.HCAs)] = true
	}
	for i := range out {
		seen := counts[out[i].Network.ID]
		if len(seen) < 2 || len(out[i].HCAs) == 0 {
			continue
		}
		var values []int
		for n := range seen {
			values = append(values, n)
		}
		sort.Ints(values)
		names := make([]string, len(values))
		for j, n := range values {
			names[j] = strconv.Itoa(n)
		}
		out[i].Warnings = append(out[i].Warnings, Warning{
			Code: WarningHCAMismatch, Params: map[string]string{"count": strconv.Itoa(len(out[i].HCAs)), "counts": strings.Join(names, ", ")},
		})
	}
}

func findInterface(inv *proto.NetInventory, name string) (proto.NetInterface, bool) {
	if name == "" {
		return proto.NetInterface{}, false
	}
	for _, iface := range inv.Interfaces {
		if iface.Name == name {
			return iface, true
		}
	}
	return proto.NetInterface{}, false
}

func firstIPv4(addrs []string) string {
	for _, addr := range addrs {
		if ip, _, err := net.ParseCIDR(addr); err == nil && ip.To4() != nil {
			return addr
		}
	}
	return ""
}
