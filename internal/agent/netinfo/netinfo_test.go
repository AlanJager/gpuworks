package netinfo

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sysfs builds the layout a mlx5 host exposes: one RoCE port bound to ens3f0,
// one IB port without IPoIB, and a netdev whose speed is unreadable.
func sysfs(t *testing.T) string {
	root := t.TempDir()
	write(t, filepath.Join(root, "class/net/ens3f0/operstate"), "up")
	write(t, filepath.Join(root, "class/net/ens3f0/speed"), "25000")
	write(t, filepath.Join(root, "class/net/eth0/operstate"), "up")
	write(t, filepath.Join(root, "class/net/eth0/speed"), "-1")
	write(t, filepath.Join(root, "class/net/eth0/mtu"), "1500")
	write(t, filepath.Join(root, "class/net/ib0/operstate"), "down")
	for _, nic := range []string{"ens3f0", "eth0", "enp1s0"} {
		if err := os.MkdirAll(filepath.Join(root, "class/net", nic, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	write(t, filepath.Join(root, "class/infiniband/mlx5_1/ports/1/state"), "4: ACTIVE")
	write(t, filepath.Join(root, "class/infiniband/mlx5_1/ports/1/rate"), "25 Gb/sec (1X EDR)")
	write(t, filepath.Join(root, "class/infiniband/mlx5_1/ports/1/link_layer"), "Ethernet")
	if err := os.MkdirAll(filepath.Join(root, "class/infiniband/mlx5_1/device/net/ens3f0"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/1/state"), "1: DOWN")
	write(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/1/rate"), "400 Gb/sec (4X NDR)")
	write(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/1/link_layer"), "InfiniBand")
	write(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/notaport/state"), "4: ACTIVE")
	return root
}

func TestCollectMergesKernelInterfacesWithSysfs(t *testing.T) {
	src := Source{SysRoot: sysfs(t), Interfaces: func() ([]Iface, error) {
		return []Iface{
			{Name: "lo", Loopback: true, Addrs: []string{"127.0.0.1/8"}},
			{Name: "eth0", MAC: "52:54:00:aa:bb:cc", Addrs: []string{"192.168.8.15/24"}},
			{Name: "ens3f0", MTU: 9000, Addrs: []string{"10.30.1.15/16", "fd00::15/64"}},
			{Name: "veth12ab"}, {Name: "docker0"}, {Name: "br-1f2e"}, {Name: "cni0"},
			// Virtual devices stay only when they carry an address.
			{Name: "vnic29.0"}, {Name: "ud_outer7"}, {Name: "br_ens3_36", Addrs: []string{"172.30.8.31/16"}},
			// A physical NIC without an address is still a candidate.
			{Name: "enp1s0"},
		}, nil
	}}
	got := src.Collect()
	wantIfaces := []proto.NetInterface{
		{Name: "br_ens3_36", Addrs: []string{"172.30.8.31/16"}},
		{Name: "enp1s0"},
		{Name: "ens3f0", State: "up", MTU: 9000, SpeedMbps: 25000, Addrs: []string{"10.30.1.15/16", "fd00::15/64"}},
		{Name: "eth0", State: "up", MTU: 1500, MAC: "52:54:00:aa:bb:cc", Addrs: []string{"192.168.8.15/24"}},
	}
	if !reflect.DeepEqual(got.Interfaces, wantIfaces) {
		t.Fatalf("interfaces = %#v\nwant %#v", got.Interfaces, wantIfaces)
	}
	wantPorts := []proto.RDMAPort{
		{Device: "mlx5_0", Port: 1, State: "DOWN", LinkLayer: "InfiniBand", RateGbps: 400},
		{Device: "mlx5_1", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", RateGbps: 25, NetDev: "ens3f0"},
	}
	if !reflect.DeepEqual(got.RDMAPorts, wantPorts) {
		t.Fatalf("rdma ports = %#v\nwant %#v", got.RDMAPorts, wantPorts)
	}
}

func TestCollectReturnsEmptyNonNilInventoryWhenSourcesFail(t *testing.T) {
	src := Source{SysRoot: filepath.Join(t.TempDir(), "missing"), Interfaces: func() ([]Iface, error) {
		return nil, errors.New("netlink unavailable")
	}}
	got := src.Collect()
	if got.Interfaces == nil || got.RDMAPorts == nil || len(got.Interfaces)+len(got.RDMAPorts) != 0 {
		t.Fatalf("inventory = %#v, want empty non-nil slices", got)
	}
}

func TestPortParsers(t *testing.T) {
	cases := map[string]float64{"400 Gb/sec (4X NDR)": 400, "2.5 Gb/sec (1X SDR)": 2.5, "": 0, "fast": 0}
	for raw, want := range cases {
		if got := portRate(raw); got != want {
			t.Fatalf("portRate(%q) = %v, want %v", raw, got, want)
		}
	}
	if got := portState("4: ACTIVE"); got != "ACTIVE" {
		t.Fatalf("portState = %q", got)
	}
	if got := portState("ACTIVE"); got != "ACTIVE" {
		t.Fatalf("portState without code = %q", got)
	}
}

func TestLiveCollectDoesNotPanic(t *testing.T) {
	inv := Collect()
	if inv.Interfaces == nil || inv.RDMAPorts == nil {
		t.Fatalf("live inventory has nil slices: %#v", inv)
	}
}

func TestCollectReportsTestTools(t *testing.T) {
	src := Source{SysRoot: t.TempDir(), LookPath: func(name string) (string, error) {
		if name == "ib_write_bw" {
			return "/usr/bin/ib_write_bw", nil
		}
		return "", errors.New("not found")
	}}
	if got := src.Collect().Tools; !reflect.DeepEqual(got, []string{"ib_write_bw"}) {
		t.Fatalf("tools = %v", got)
	}
	src.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if got := src.Collect().Tools; got != nil {
		t.Fatalf("tools without any = %v", got)
	}
}
