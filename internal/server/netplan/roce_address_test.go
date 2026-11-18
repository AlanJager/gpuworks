package netplan

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRoCEAddressValidationKeepsLegacyAndPhysicalRateFallbacks(t *testing.T) {
	inv := &proto.NetInventory{Interfaces: []proto.NetInterface{
		{Name: "physical", Addrs: []string{"198.51.100.12/24"}, SpeedMbps: 25000},
		{Name: "bond0", Addrs: []string{"192.0.2.12/24", "2001:db8::12/64"}, SpeedMbps: 200000},
	}}
	for _, tc := range []struct {
		name                 string
		address              *proto.RoCEAddress
		layer                string
		wantNetdev, wantAddr string
		wantSpeed            float64
	}{
		{"old agent", nil, "Ethernet", "physical", "198.51.100.12/24", 25},
		{"valid match", &proto.RoCEAddress{NetDev: "bond0", CIDR: "192.0.2.12/24"}, "Ethernet", "bond0", "192.0.2.12/24", 25},
		{"stale address", &proto.RoCEAddress{NetDev: "bond0", CIDR: "192.0.2.99/24"}, "Ethernet", "physical", "198.51.100.12/24", 25},
		{"missing interface", &proto.RoCEAddress{NetDev: "gone", CIDR: "192.0.2.12/24"}, "Ethernet", "physical", "198.51.100.12/24", 25},
		{"invalid CIDR", &proto.RoCEAddress{NetDev: "bond0", CIDR: "192.0.2.12"}, "Ethernet", "physical", "198.51.100.12/24", 25},
		{"IPv6 excluded", &proto.RoCEAddress{NetDev: "bond0", CIDR: "2001:db8::12/64"}, "Ethernet", "physical", "198.51.100.12/24", 25},
		{"InfiniBand unchanged", &proto.RoCEAddress{NetDev: "bond0", CIDR: "192.0.2.12/24"}, "InfiniBand", "physical", "198.51.100.12/24", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := proto.RDMAPort{Device: "hca", Port: 1, NetDev: "physical", State: "ACTIVE", LinkLayer: tc.layer, RateGbps: 100, RoCEAddress: tc.address}
			rail := describeRail(inv, port)
			if rail.NetDev != tc.wantNetdev || rail.Addr != tc.wantAddr || rail.SpeedGbps != tc.wantSpeed {
				t.Fatalf("unexpected address or physical speed: %+v", rail)
			}
		})
	}
}
