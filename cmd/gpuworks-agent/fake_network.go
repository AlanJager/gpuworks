package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/proto"
)

// fakeNetworkEnv names a JSON file holding the network inventory a process
// test's agent reports instead of the host's (proto.NetInventory): several
// agents on one test host then stand for nodes with their own addresses and
// RDMA ports. Production startup never sets it.
const fakeNetworkEnv = "GPUWORKS_FAKE_NETWORK"

// applyFakeNetwork makes the agent report the inventory in the file and
// resolve its addresses to the interfaces the file gives them.
func applyFakeNetwork(cfg *agentapp.Config, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", fakeNetworkEnv, err)
	}
	var inv proto.NetInventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		return fmt.Errorf("%s %s: %w", fakeNetworkEnv, path, err)
	}
	if inv.Management == nil || inv.Management.IP == "" {
		return fmt.Errorf("%s %s: the inventory needs a management address", fakeNetworkEnv, path)
	}
	if inv.Interfaces == nil {
		inv.Interfaces = []proto.NetInterface{}
	}
	if inv.RDMAPorts == nil {
		inv.RDMAPorts = []proto.RDMAPort{}
	}
	cfg.NetworkInventory = func() proto.NetInventory { return inv }
	cfg.NetworkInterfaceResolver = func(ip string) (string, error) {
		return interfaceOf(inv, ip)
	}
	return nil
}

// interfaceOf is the interface in the inventory that holds the address.
func interfaceOf(inv proto.NetInventory, ip string) (string, error) {
	for _, iface := range inv.Interfaces {
		for _, addr := range iface.Addrs {
			host, _, err := net.ParseCIDR(addr)
			if err != nil {
				host = net.ParseIP(strings.TrimSpace(addr))
			}
			if host != nil && host.String() == ip {
				return iface.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no interface in %s holds %s", fakeNetworkEnv, ip)
}
