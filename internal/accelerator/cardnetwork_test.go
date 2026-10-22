package accelerator

import (
	"reflect"
	"testing"
)

func TestCardNetworkOf(t *testing.T) {
	cards, ok := CardNetworkOf(" Ascend ")
	if !ok || cards.RailPrefix != "npu" || !reflect.DeepEqual(cards.AddressEnv, []string{"HCCL_IF_IP"}) ||
		!reflect.DeepEqual(cards.InterfaceEnv, []string{"HCCL_SOCKET_IFNAME"}) || !reflect.DeepEqual(cards.HostFiles, []string{"/etc/hccn.conf"}) {
		t.Fatalf("ascend = %+v %v", cards, ok)
	}
	for _, vendor := range []string{VendorNVIDIA, VendorAMD, VendorPPU, VendorHygon, "", "unknown"} {
		if cards, ok := CardNetworkOf(vendor); ok || !reflect.DeepEqual(cards, CardNetwork{}) {
			t.Fatalf("%q has a card network: %+v", vendor, cards)
		}
	}
}

func TestPortRailNames(t *testing.T) {
	if got := PortRailName(VendorAscend, 3); got != "npu3" {
		t.Fatalf("name = %q", got)
	}
	if got := PortRailName(VendorNVIDIA, 3); got != "" {
		t.Fatalf("nvidia name = %q", got)
	}
	for name, want := range map[string]int{"npu0": 0, "npu3": 3, "npu10": 10} {
		if got, ok := PortRailIndex(VendorAscend, name); !ok || got != want || PortRailName(VendorAscend, got) != name {
			t.Fatalf("%s = %d %v", name, got, ok)
		}
	}
	// Anything PortRailName would not produce is not a card.
	for _, name := range []string{"", "npu", "npu-1", "npu03", "npu3 ", "npu3:1", "mlx5_0:1", "gpu3", "npux"} {
		if got, ok := PortRailIndex(VendorAscend, name); ok {
			t.Fatalf("%q read as card %d", name, got)
		}
	}
	if _, ok := PortRailIndex(VendorNVIDIA, "npu3"); ok {
		t.Fatal("a vendor without card ports has a card")
	}
}
