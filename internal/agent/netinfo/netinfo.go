// Package netinfo reports the host network inventory: IP interfaces from the
// kernel, link/RDMA facts from sysfs, and the TCP listeners inside the engine
// port range from netlink sock_diag. It never shells out and needs no cgo,
// matching the rest of the agent's host probing.
package netinfo

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Iface is the kernel view of one interface; it exists so tests can supply
// interfaces without owning the host network namespace.
type Iface struct {
	Name     string
	MAC      string
	MTU      int
	Loopback bool
	Addrs    []string // CIDR strings
}

// Source reads the inventory. SysRoot is the sysfs mount ("/sys" in
// production) and Interfaces lists kernel interfaces with their addresses.
type Source struct {
	SysRoot    string
	Interfaces func() ([]Iface, error)
	// OS is the GOOS whose virtual interface names apply; empty applies
	// only the names every OS shares (tests set it, Collect passes this host's).
	OS string
	// LookPath finds network test tools; nil reports none.
	LookPath func(string) (string, error)
}

// testTools are the host tools the server's network tests can drive.
var testTools = []string{"ib_write_bw"}

// Collect reads the live host inventory.
func Collect() proto.NetInventory {
	return Source{SysRoot: "/sys", Interfaces: hostInterfaces, LookPath: exec.LookPath, OS: goruntime.GOOS}.Collect()
}

// virtualPrefixes are container and overlay plumbing interfaces. They never
// carry node-to-node traffic that a network selector should pick.
var virtualPrefixes = []string{
	"veth", "docker", "br-", "cni", "flannel", "cali", "virbr", "vxlan",
	"tunl", "kube-ipvs", "nerdctl", "cilium", "weave", "lxc",
}

// Collect is best-effort: an unreadable source yields an empty, non-nil
// inventory so the server can tell "nothing found" from an older agent.
func (s Source) Collect() proto.NetInventory {
	inv := proto.NetInventory{Interfaces: []proto.NetInterface{}, RDMAPorts: []proto.RDMAPort{}}
	if s.Interfaces != nil {
		ifaces, err := s.Interfaces()
		if err == nil {
			for _, iface := range ifaces {
				if iface.Loopback || isVirtual(iface.Name) || (s.OS == "darwin" && isMacVirtual(iface.Name)) || !s.worthReporting(iface) {
					continue
				}
				inv.Interfaces = append(inv.Interfaces, s.describe(iface))
			}
		}
	}
	sort.Slice(inv.Interfaces, func(i, j int) bool { return inv.Interfaces[i].Name < inv.Interfaces[j].Name })
	inv.RDMAPorts = s.rdmaPorts()
	if s.LookPath != nil {
		for _, tool := range testTools {
			if _, err := s.LookPath(tool); err == nil {
				inv.Tools = append(inv.Tools, tool)
			}
		}
	}
	return inv
}

// worthReporting keeps physical NICs, and virtual ones (bridges, bonds,
// VLANs) only when they carry an address: an address-less tap or veth-like
// device cannot be a network endpoint and would bury the real NICs on hosts
// that also run VMs or Kubernetes.
func (s Source) worthReporting(iface Iface) bool {
	if len(iface.Addrs) > 0 {
		return true
	}
	_, err := os.Stat(filepath.Join(s.SysRoot, "class", "net", iface.Name, "device"))
	return err == nil
}

// macVirtualPrefixes are a Mac's VM and peer-to-peer plumbing: bridges that
// OrbStack, UTM and Internet Sharing create (bridge100 holds the VMs'
// gateway address), VM network interfaces, and Apple's AWDL/low-latency
// WLAN, NAN and USB-to-host links. utun stays: Tailscale is one.
var macVirtualPrefixes = []string{"bridge", "vmenet", "vmnet", "feth", "awdl", "llw", "anpi", "nan", "gif", "stf", "ap"}

func isMacVirtual(name string) bool {
	for _, prefix := range macVirtualPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func isVirtual(name string) bool {
	for _, prefix := range virtualPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (s Source) describe(iface Iface) proto.NetInterface {
	dir := filepath.Join(s.SysRoot, "class", "net", iface.Name)
	out := proto.NetInterface{
		Name:  iface.Name,
		State: readString(filepath.Join(dir, "operstate")),
		MTU:   iface.MTU,
		MAC:   iface.MAC,
		Addrs: append([]string(nil), iface.Addrs...),
	}
	if out.MTU == 0 {
		out.MTU = readInt(filepath.Join(dir, "mtu"))
	}
	// speed reads -1 or fails with EINVAL on links that are down or virtual.
	if speed := readInt(filepath.Join(dir, "speed")); speed > 0 {
		out.SpeedMbps = speed
	}
	return out
}

func (s Source) rdmaPorts() []proto.RDMAPort {
	ports := []proto.RDMAPort{}
	root := filepath.Join(s.SysRoot, "class", "infiniband")
	devices, err := os.ReadDir(root)
	if err != nil {
		return ports
	}
	for _, device := range devices {
		name := device.Name()
		netdev := firstEntry(filepath.Join(root, name, "device", "net"))
		portEntries, err := os.ReadDir(filepath.Join(root, name, "ports"))
		if err != nil {
			continue
		}
		for _, portEntry := range portEntries {
			number, err := strconv.Atoi(portEntry.Name())
			if err != nil {
				continue
			}
			dir := filepath.Join(root, name, "ports", portEntry.Name())
			ports = append(ports, proto.RDMAPort{
				Device:    name,
				Port:      number,
				State:     portState(readString(filepath.Join(dir, "state"))),
				LinkLayer: readString(filepath.Join(dir, "link_layer")),
				RateGbps:  portRate(readString(filepath.Join(dir, "rate"))),
				NetDev:    netdev,
			})
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Device != ports[j].Device {
			return ports[i].Device < ports[j].Device
		}
		return ports[i].Port < ports[j].Port
	})
	return ports
}

// portState turns sysfs "4: ACTIVE" into "ACTIVE".
func portState(raw string) string {
	if _, state, ok := strings.Cut(raw, ":"); ok {
		return strings.TrimSpace(state)
	}
	return raw
}

// portRate turns sysfs "400 Gb/sec (4X NDR)" into 400 and "2.5 Gb/sec" into 2.5.
func portRate(raw string) float64 {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0
	}
	rate, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return rate
}

func firstEntry(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return ""
	}
	return entries[0].Name()
}

func readString(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func readInt(path string) int {
	value, err := strconv.Atoi(readString(path))
	if err != nil {
		return 0
	}
	return value
}

func hostInterfaces() ([]Iface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Iface, 0, len(ifaces))
	for _, iface := range ifaces {
		item := Iface{
			Name:     iface.Name,
			MAC:      iface.HardwareAddr.String(),
			MTU:      iface.MTU,
			Loopback: iface.Flags&net.FlagLoopback != 0,
		}
		addrs, err := iface.Addrs()
		if err == nil {
			for _, addr := range addrs {
				if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLinkLocalUnicast() {
					item.Addrs = append(item.Addrs, ipNet.String())
				}
			}
		}
		out = append(out, item)
	}
	return out, nil
}
