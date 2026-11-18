package netinventory_test

import (
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/netinfo"
	"gpuworks.ai/gpuworks/internal/server/netplan"
)

func put(t *testing.T, root, path, value string) {
	t.Helper()
	file := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(value+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestAddressBearingGIDEnablesAutoRail(t *testing.T) {
	for _, name := range []string{"br_bond2", "bond0", "bond0.100"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "class/net/ens3f0/device/present", "")
			put(t, root, "class/net/ens3f0/speed", "100000")
			put(t, root, "class/infiniband/mlx5_0/device/net/ens3f0/present", "")
			port := "class/infiniband/mlx5_0/ports/1/"
			for path, value := range map[string]string{"link_layer": "Ethernet", "state": "4: ACTIVE", "rate": "100 Gb/sec", "gids/3": "::ffff:192.0.2.12", "gid_attrs/ndevs/3": name, "gid_attrs/types/3": "RoCE v2"} {
				put(t, root, port+path, value)
			}
			inv := (netinfo.Source{SysRoot: root, Interfaces: func() ([]netinfo.Iface, error) {
				return []netinfo.Iface{{Name: "ens3f0"}, {Name: name, Addrs: []string{"192.0.2.12/24"}}}, nil
			}}).Collect()
			rails := netplan.AllRails(&inv, netplan.TypeHostRDMA)
			if len(rails) != 1 {
				t.Fatalf("rail count=%d", len(rails))
			}
			out := netplan.Resolve(netplan.Inputs{Nodes: []netplan.Node{{ID: 1, Inventory: &inv}}, Networks: []netplan.Network{{ID: 1, Name: "compute", Type: netplan.TypeHostRDMA, Roles: []string{netplan.RoleCompute}}}, Attachments: []netplan.Attachment{{ID: 1, NodeID: 1, NetworkID: 1}}})
			for _, selected := range out {
				if selected.Role == netplan.RoleCompute {
					t.Logf("netdev=%s gap=%q address=%q auto_hcas=%v bootstrap=%q", rails[0].NetDev, rails[0].Gap(), rails[0].Addr, selected.HCAs, selected.IP)
					if rails[0].Gap() != "" || rails[0].NetDev != name || len(selected.HCAs) != 1 || selected.IP != "192.0.2.12" {
						t.Fatal("address-bearing GID did not produce a usable rail and bootstrap")
					}
					return
				}
			}
			t.Fatal("compute resolution missing")
		})
	}
}
