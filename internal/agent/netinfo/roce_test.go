package netinfo

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRoCEGIDResolvesBridgeInsteadOfAddresslessPhysicalNIC(t *testing.T) {
	root := sysfs(t)
	dir := filepath.Join(root, "class/infiniband/mlx5_1/ports/1")
	for index, gid := range map[string]string{"0": "fe80::1234", "2": "::ffff:192.0.2.12", "3": "::ffff:192.0.2.12"} {
		write(t, filepath.Join(dir, "gids", index), gid)
		name := "br_bond2"
		if index == "0" {
			name = "ens3f0"
		}
		write(t, filepath.Join(dir, "gid_attrs/ndevs", index), name)
		write(t, filepath.Join(dir, "gid_attrs/types", index), "RoCE v2")
	}
	src := Source{SysRoot: root, Interfaces: func() ([]Iface, error) {
		return []Iface{{Name: "ens3f0", Addrs: []string{"fe80::1234/64"}}, {Name: "br_bond2", Addrs: []string{"192.0.2.12/24"}}}, nil
	}}
	got := src.Collect()
	if len(got.RDMAPorts) != 2 || got.RDMAPorts[1].NetDev != "ens3f0" || got.RDMAPorts[1].RoCEAddress == nil || *got.RDMAPorts[1].RoCEAddress != (proto.RoCEAddress{NetDev: "br_bond2", CIDR: "192.0.2.12/24"}) {
		t.Fatalf("bridge GID not resolved: %+v", got.RDMAPorts)
	}
}

func TestRoCEGIDSelectionRejectsStaleAndUnrelatedAddresses(t *testing.T) {
	dir := t.TempDir()
	set := func(index, name, gid, typ string) {
		write(t, filepath.Join(dir, "gids", index), gid)
		write(t, filepath.Join(dir, "gid_attrs/ndevs", index), name)
		write(t, filepath.Join(dir, "gid_attrs/types", index), typ)
	}
	ifaces := []proto.NetInterface{{Name: "bridge", Addrs: []string{"192.0.2.12/24"}}, {Name: "vlan", Addrs: []string{"198.51.100.12/24"}}}
	set("0", "bridge", "fe80::1", "RoCE v2")
	set("1", "missing", "::ffff:192.0.2.12", "RoCE v2")
	set("2", "bridge", "::ffff:192.0.2.99", "RoCE v2")
	set("3", "bridge", "bad", "RoCE v2")
	if got := roceGIDAddress(dir, ifaces, "physical"); got != nil {
		t.Fatalf("invalid GID selected: %+v", got)
	}
	set("4", "bridge", "::ffff:192.0.2.12", "IB/RoCE v1")
	set("10", "bridge", "::ffff:192.0.2.12", "RoCE v2")
	set("5", "vlan", "::ffff:198.51.100.12", "RoCE v2")
	if got := roceGIDAddress(dir, ifaces, "physical"); got == nil || *got != (proto.RoCEAddress{NetDev: "vlan", CIDR: "198.51.100.12/24"}) {
		t.Fatalf("expected first numeric v2 GID, got %+v", got)
	}
}

func TestRoCEGIDFallbackAndPortIsolation(t *testing.T) {
	root := sysfs(t)
	port := filepath.Join(root, "class/infiniband/mlx5_1/ports/1")
	physical := "ens3f0"
	if got := roceGIDAddress(port, nil, physical); got != nil {
		t.Fatal(got)
	}
	write(t, filepath.Join(port, "gids/4"), "::ffff:192.0.2.12")
	write(t, filepath.Join(port, "gid_attrs/ndevs/4"), "bond0.100")
	write(t, filepath.Join(port, "gid_attrs/types/4"), "RoCE v2")
	for _, addr := range []string{"", "bad", "192.0.2.99/24", "2001:db8::12/64"} {
		if got := roceGIDAddress(port, []proto.NetInterface{{Name: "bond0.100", Addrs: []string{addr}}}, physical); got != nil {
			t.Fatalf("address %q: %+v", addr, got)
		}
	}
	src := Source{SysRoot: root, Interfaces: func() ([]Iface, error) {
		return []Iface{{Name: physical}, {Name: "bond0.100", Addrs: []string{"192.0.2.12/24"}}}, nil
	}}
	got := src.Collect()
	if len(got.RDMAPorts) != 2 || got.RDMAPorts[0].RoCEAddress != nil || got.RDMAPorts[1].NetDev != physical || got.RDMAPorts[1].RoCEAddress == nil || got.RDMAPorts[1].RoCEAddress.NetDev != "bond0.100" {
		t.Fatalf("port association leaked: %+v", got.RDMAPorts)
	}
	// InfiniBand ports keep their original association even if a driver exposes
	// similar GID files; this patch changes Ethernet/RoCE only.
	write(t, filepath.Join(port, "link_layer"), "InfiniBand")
	got = src.Collect()
	if got.RDMAPorts[1].NetDev != physical || got.RDMAPorts[1].RoCEAddress != nil {
		t.Fatalf("IB changed: %+v", got.RDMAPorts[1])
	}
}

func TestRoCEGIDPrefersPhysicalIPv4WithoutBlockingOnIPv6(t *testing.T) {
	for _, tc := range []struct{ name, gid, address, stale string }{
		{"ipv4", "::ffff:198.51.100.12", "198.51.100.12/24", "198.51.100.99/24"},
		{"ipv6", "2001:db8::12", "2001:db8::12/64", "2001:db8::99/64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, row := range []struct{ index, name, gid, kind string }{
				{"1", "bridge", "::ffff:192.0.2.12", "RoCE v2"},
				{"7", "physical", tc.gid, "IB/RoCE v1"},
			} {
				write(t, filepath.Join(dir, "gids", row.index), row.gid)
				write(t, filepath.Join(dir, "gid_attrs/ndevs", row.index), row.name)
				write(t, filepath.Join(dir, "gid_attrs/types", row.index), row.kind)
			}
			ifaces := []proto.NetInterface{{Name: "physical", Addrs: []string{tc.address}}, {Name: "bridge", Addrs: []string{"192.0.2.12/24"}}}
			if got := roceGIDAddress(dir, ifaces, "physical"); (tc.name == "ipv4" && (got == nil || *got != (proto.RoCEAddress{NetDev: "physical", CIDR: tc.address}))) || (tc.name == "ipv6" && (got == nil || got.NetDev != "bridge")) {
				t.Fatalf("valid network association changed: %+v", got)
			}
			ifaces[0].Addrs = []string{tc.stale}
			if got := roceGIDAddress(dir, ifaces, "physical"); got == nil || got.NetDev != "bridge" {
				t.Fatalf("stale physical association retained: %+v", got)
			}
		})
	}
}
