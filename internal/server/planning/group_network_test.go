package planning

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func networkGroup(typ string, cards map[uint][]string) netplan.Group {
	group := netplan.Group{Network: netplan.Network{ID: 3, Name: "shared", Type: typ}, Ranks: map[uint]netplan.Resolved{}}
	if typ == netplan.TypeHostEthernet {
		group.Network = netplan.Management()
	}
	for id, rails := range cards {
		group.Ranks[id] = netplan.Resolved{NodeID: id, IP: "10.0.0.1", HCAs: rails}
	}
	return group
}

func productCodeAndParams(t *testing.T, err error) (string, map[string]any) {
	t.Helper()
	if err == nil {
		return "", nil
	}
	var product *messages.ProductError
	if !errors.As(err, &product) {
		t.Fatalf("err = %v, want a product error", err)
	}
	return product.Code(), product.Params()
}

func TestCheckGroupNetwork(t *testing.T) {
	ascend := []store.Node{{ID: 1, UUID: "a", Name: "huawei31", GPUVendor: "ascend"}, {ID: 2, UUID: "b", Name: "huawei32", GPUVendor: "Ascend"}}
	nvidia := []store.Node{{ID: 1, UUID: "a", Name: "gpu-a", GPUVendor: "nvidia"}, {ID: 2, UUID: "b", Name: "gpu-b", GPUVendor: "nvidia"}}
	bothCards := map[uint][]string{1: {"npu3"}, 2: {"npu3", "npu4"}}
	for name, tc := range map[string]struct {
		nodes  []store.Node
		group  netplan.Group
		code   string
		params map[string]any
	}{
		"ascend on cards":            {ascend, networkGroup(netplan.TypeAcceleratorRDMA, bothCards), "", nil},
		"ascend on the management":   {ascend, networkGroup(netplan.TypeHostEthernet, map[uint][]string{1: nil, 2: nil}), messages.ErrorComputeNetworkAcceleratorRequired, map[string]any{"node": "huawei31", "network": "management"}},
		"ascend on host RDMA":        {ascend, networkGroup(netplan.TypeHostRDMA, map[uint][]string{1: {"rocep61s0f0:1"}, 2: {"rocep61s0f0:1"}}), messages.ErrorComputeNetworkAcceleratorRequired, map[string]any{"node": "huawei31", "network": "shared"}},
		"a node without cards":       {ascend, networkGroup(netplan.TypeAcceleratorRDMA, map[uint][]string{1: {"npu3"}, 2: nil}), messages.ErrorComputeNetworkNoCards, map[string]any{"node": "huawei32", "network": "shared"}},
		"nvidia on host RDMA":        {nvidia, networkGroup(netplan.TypeHostRDMA, map[uint][]string{1: {"mlx5_0:1"}, 2: {"mlx5_0:1"}}), "", nil},
		"nvidia on the management":   {nvidia, networkGroup(netplan.TypeHostEthernet, map[uint][]string{1: nil, 2: nil}), "", nil},
		"nvidia on an accelerator":   {nvidia, networkGroup(netplan.TypeAcceleratorRDMA, map[uint][]string{1: nil, 2: nil}), messages.ErrorComputeNetworkNoCards, map[string]any{"node": "gpu-a", "network": "shared"}},
		"one ascend node, any":       {ascend[:1], networkGroup(netplan.TypeHostEthernet, map[uint][]string{1: nil}), "", nil},
		"mixed vendors, host":        {[]store.Node{nvidia[0], ascend[1]}, networkGroup(netplan.TypeHostRDMA, bothCards), messages.ErrorComputeNetworkAcceleratorRequired, map[string]any{"node": "huawei32", "network": "shared"}},
		"no nodes":                   {nil, netplan.Group{}, "", nil},
		"udocker on host RDMA":       {[]store.Node{nvidia[0], {ID: 2, UUID: "b", Name: "gpu-b", GPUVendor: "nvidia", RuntimeDriver: "udocker"}}, networkGroup(netplan.TypeHostRDMA, bothCards), messages.ErrorComputeNetworkRuntime, map[string]any{"node": "gpu-b", "network": "shared", "runtime": "udocker"}},
		"udocker on host Ethernet":   {[]store.Node{nvidia[0], {ID: 2, UUID: "b", Name: "gpu-b", GPUVendor: "nvidia", RuntimeDriver: "udocker"}}, networkGroup(netplan.TypeHostEthernet, map[uint][]string{1: nil, 2: nil}), "", nil},
		"unknown vendor is not held": {[]store.Node{{ID: 1, Name: "x"}, {ID: 2, Name: "y"}}, networkGroup(netplan.TypeHostEthernet, map[uint][]string{1: nil, 2: nil}), "", nil},
	} {
		code, params := productCodeAndParams(t, CheckGroupNetwork(tc.nodes, tc.group))
		if code != tc.code || !reflect.DeepEqual(params, tc.params) {
			t.Errorf("%s: %s %v, want %s %v", name, code, params, tc.code, tc.params)
		}
	}
}

func TestCheckPDNetwork(t *testing.T) {
	hccl, ok := pdmode.Lookup("vllm-ascend-mooncake")
	if !ok || hccl.Transport != pdmode.TransportHCCL {
		t.Fatalf("vllm-ascend-mooncake = %+v %v", hccl, ok)
	}
	ucx, ok := pdmode.Lookup("vllm-nixl")
	if !ok || ucx.Transport != pdmode.TransportUCX {
		t.Fatalf("vllm-nixl = %+v %v", ucx, ok)
	}
	two := []store.Node{{ID: 1, Name: "n1"}, {ID: 2, Name: "n2"}}
	one := two[:1]
	cards := map[uint][]string{1: {"npu3"}, 2: {"npu3"}}
	none := map[uint][]string{1: nil, 2: nil}
	for name, tc := range map[string]struct {
		mode   pdmode.Mode
		nodes  []store.Node
		group  netplan.Group
		code   string
		params map[string]any
	}{
		"HCCL across hosts on cards":          {hccl, two, networkGroup(netplan.TypeAcceleratorRDMA, cards), "", nil},
		"HCCL across hosts on the management": {hccl, two, networkGroup(netplan.TypeHostEthernet, none), messages.ErrorPDNetworkAcceleratorRequired, map[string]any{"mode": "vllm-ascend-mooncake", "network": "management"}},
		"HCCL across hosts on host RDMA":      {hccl, two, networkGroup(netplan.TypeHostRDMA, none), messages.ErrorPDNetworkAcceleratorRequired, map[string]any{"mode": "vllm-ascend-mooncake", "network": "shared"}},
		// On one host the KV never leaves it, whatever network it is on, or
		// none (PDGroup's loopback group).
		"HCCL on one host, management":            {hccl, one, networkGroup(netplan.TypeHostEthernet, none), "", nil},
		"HCCL on one host, loopback":              {hccl, one, netplan.Group{Ranks: map[uint]netplan.Resolved{1: {NodeID: 1, IP: "127.0.0.1"}}}, "", nil},
		"HCCL on one host, cards":                 {hccl, one, networkGroup(netplan.TypeAcceleratorRDMA, cards), "", nil},
		"HCCL across hosts, a node without cards": {hccl, two, networkGroup(netplan.TypeAcceleratorRDMA, map[uint][]string{1: {"npu3"}, 2: nil}), messages.ErrorComputeNetworkNoCards, map[string]any{"node": "n2", "network": "shared"}},
		"UCX on host RDMA":                        {ucx, two, networkGroup(netplan.TypeHostRDMA, none), "", nil},
		"UCX on the management":                   {ucx, two, networkGroup(netplan.TypeHostEthernet, none), "", nil},
		"UCX on cards":                            {ucx, two, networkGroup(netplan.TypeAcceleratorRDMA, cards), messages.ErrorPDNetworkHostRequired, map[string]any{"mode": "vllm-nixl", "network": "shared"}},
		"UCX on cards, one host":                  {ucx, one, networkGroup(netplan.TypeAcceleratorRDMA, cards), messages.ErrorPDNetworkHostRequired, map[string]any{"mode": "vllm-nixl", "network": "shared"}},
		// A sleepable deployment cannot register its KV cache with an RDMA
		// NIC, on one host or across hosts; over Ethernet, on one host's
		// loopback, or with HCCL on the cards it sleeps.
		"sleepable UCX on host RDMA":           {ucx, two, networkGroup(netplan.TypeHostRDMA, none), messages.ErrorPDSleepRDMAUnsupported, map[string]any{"mode": "vllm-nixl", "network": "shared"}},
		"sleepable UCX on host RDMA, one host": {ucx, one, networkGroup(netplan.TypeHostRDMA, none), messages.ErrorPDSleepRDMAUnsupported, map[string]any{"mode": "vllm-nixl", "network": "shared"}},
		"sleepable UCX on the management":      {ucx, two, networkGroup(netplan.TypeHostEthernet, none), "", nil},
		"sleepable UCX on one host, loopback":  {ucx, one, netplan.Group{Ranks: map[uint]netplan.Resolved{1: {NodeID: 1, IP: "127.0.0.1"}}}, "", nil},
		"sleepable HCCL across hosts on cards": {hccl, two, networkGroup(netplan.TypeAcceleratorRDMA, cards), "", nil},
	} {
		sleepable := strings.HasPrefix(name, "sleepable ")
		code, params := productCodeAndParams(t, CheckPDNetwork(tc.mode, tc.nodes, tc.group, sleepable))
		if code != tc.code || !reflect.DeepEqual(params, tc.params) {
			t.Errorf("%s: %s %v, want %s %v", name, code, params, tc.code, tc.params)
		}
	}
}
