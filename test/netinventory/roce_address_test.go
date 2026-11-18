package netinventory_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/netinfo"
	"gpuworks.ai/gpuworks/internal/server/netplan"
)

func TestRoCEBondRateRemainsPerPort(t *testing.T) {
	checkRoCECarrier(t, []string{"192.0.2.12/24"}, "200000", true)
}

func TestRoCEBootstrapUsesTheMatchedGIDAddress(t *testing.T) {
	checkRoCECarrier(t, []string{"198.51.100.12/24", "192.0.2.12/24"}, "100000", false)
}

func checkRoCECarrier(t *testing.T, addrs []string, speed string, bandwidth bool) {
	t.Helper()
	root := t.TempDir()
	put(t, root, "class/net/ens3f0/device/present", "")
	put(t, root, "class/net/ens3f0/speed", "100000")
	put(t, root, "class/net/bond0/speed", speed)
	put(t, root, "class/infiniband/mlx5_0/device/net/ens3f0/present", "")
	port := "class/infiniband/mlx5_0/ports/1/"
	for p, v := range map[string]string{"link_layer": "Ethernet", "state": "4: ACTIVE", "rate": "100 Gb/sec", "gids/3": "::ffff:192.0.2.12", "gid_attrs/ndevs/3": "bond0", "gid_attrs/types/3": "RoCE v2"} {
		put(t, root, port+p, v)
	}
	inv := (netinfo.Source{SysRoot: root, Interfaces: func() ([]netinfo.Iface, error) {
		return []netinfo.Iface{{Name: "ens3f0"}, {Name: "bond0", Addrs: addrs}}, nil
	}}).Collect()
	rails := netplan.AllRails(&inv, netplan.TypeHostRDMA)
	if len(rails) != 1 {
		t.Fatal("missing rail")
	}
	if bandwidth {
		if rails[0].SpeedGbps != 100 {
			t.Fatalf("bond aggregate leaked into physical rail: got %g Gbps, want 100", rails[0].SpeedGbps)
		}
		return
	}
	if rails[0].IP() != "192.0.2.12" {
		t.Fatalf("address does not match GID: got %q, want 192.0.2.12", rails[0].IP())
	}
}
