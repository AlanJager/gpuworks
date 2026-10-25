package engine

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestApplyDistributedNetworkInterface(t *testing.T) {
	ws := runtime.WorkloadSpec{Env: map[string]string{
		vllmHostIPEnv:        "10.60.119.97",
		"GLOO_SOCKET_IFNAME": "",
		"NCCL_SOCKET_IFNAME": "",
	}}
	spec := proto.InstanceDesiredState{Engine: "vllm", NNodes: 2, HostNetwork: true}
	calls := 0
	err := applyDistributedNetworkInterface(&ws, spec, accelerator.VendorNVIDIA, func(ip string) (string, error) {
		calls++
		if ip != "10.60.119.97" {
			t.Fatalf("resolver IP = %q", ip)
		}
		return "ens5", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
	for _, key := range socketInterfaceEnvFor(accelerator.VendorNVIDIA) {
		if got := ws.Env[key]; got != "ens5" {
			t.Fatalf("%s = %q, want ens5", key, got)
		}
	}
}

func TestApplyDistributedNetworkInterfacePreservesOverrides(t *testing.T) {
	ws := runtime.WorkloadSpec{Env: map[string]string{
		vllmHostIPEnv:        "10.60.119.97",
		"GLOO_SOCKET_IFNAME": "private0",
		"NCCL_SOCKET_IFNAME": "^docker,lo",
	}}
	spec := proto.InstanceDesiredState{Engine: "vllm", NNodes: 2, HostNetwork: true}
	err := applyDistributedNetworkInterface(&ws, spec, accelerator.VendorNVIDIA, func(string) (string, error) {
		t.Fatal("resolver called despite explicit overrides")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Env["GLOO_SOCKET_IFNAME"] != "private0" || ws.Env["NCCL_SOCKET_IFNAME"] != "^docker,lo" {
		t.Fatalf("overrides changed: %+v", ws.Env)
	}
}

func TestApplyDistributedNetworkInterfaceFillsOnlyMissingValue(t *testing.T) {
	ws := runtime.WorkloadSpec{Env: map[string]string{
		vllmHostIPEnv:        "10.60.119.97",
		"GLOO_SOCKET_IFNAME": "private0",
	}}
	spec := proto.InstanceDesiredState{Engine: "vllm", NNodes: 2, HostNetwork: true}
	if err := applyDistributedNetworkInterface(&ws, spec, accelerator.VendorNVIDIA, func(string) (string, error) {
		return "ens5", nil
	}); err != nil {
		t.Fatal(err)
	}
	if ws.Env["GLOO_SOCKET_IFNAME"] != "private0" || ws.Env["NCCL_SOCKET_IFNAME"] != "ens5" {
		t.Fatalf("unexpected env: %+v", ws.Env)
	}
}

func TestApplyDistributedNetworkInterfaceRejectsUnresolvableTopology(t *testing.T) {
	tests := []struct {
		name string
		ws   runtime.WorkloadSpec
		spec proto.InstanceDesiredState
		want string
	}{
		{
			name: "container network",
			ws:   runtime.WorkloadSpec{Env: map[string]string{vllmHostIPEnv: "10.0.0.2"}},
			spec: proto.InstanceDesiredState{Engine: "vllm", NNodes: 2},
			want: "requires host networking",
		},
		{
			name: "missing advertised address",
			ws:   runtime.WorkloadSpec{Env: map[string]string{}},
			spec: proto.InstanceDesiredState{Engine: "vllm", NNodes: 2, HostNetwork: true},
			want: vllmHostIPEnv + " is empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := applyDistributedNetworkInterface(&test.ws, test.spec, accelerator.VendorNVIDIA, func(string) (string, error) {
				return "", errors.New("not found")
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestApplyDistributedNetworkInterfaceReportsResolverFailure(t *testing.T) {
	ws := runtime.WorkloadSpec{Env: map[string]string{vllmHostIPEnv: "10.0.0.2"}}
	spec := proto.InstanceDesiredState{Engine: "vllm", NNodes: 2, HostNetwork: true}
	err := applyDistributedNetworkInterface(&ws, spec, accelerator.VendorNVIDIA, func(string) (string, error) {
		return "", errors.New("no matching interface")
	})
	if err == nil || !strings.Contains(err.Error(), "no matching interface") {
		t.Fatalf("error = %v", err)
	}
}

func TestApplyDistributedNetworkInterfaceIgnoresOtherWorkloads(t *testing.T) {
	for _, spec := range []proto.InstanceDesiredState{
		{Engine: "vllm", NNodes: 1},
		{Engine: "sglang", NNodes: 2},
	} {
		ws := runtime.WorkloadSpec{Env: map[string]string{"NCCL_SOCKET_IFNAME": "lo"}}
		if err := applyDistributedNetworkInterface(&ws, spec, accelerator.VendorNVIDIA, func(string) (string, error) {
			t.Fatal("resolver called for an out-of-scope workload")
			return "", nil
		}); err != nil {
			t.Fatal(err)
		}
		if ws.Env["NCCL_SOCKET_IFNAME"] != "lo" {
			t.Fatalf("out-of-scope workload env changed: %+v", ws.Env)
		}
	}
}

func TestNetworkInterfaceNameForIPRejectsInvalidAndLoopback(t *testing.T) {
	if _, err := networkInterfaceNameForIP("not-an-ip"); err == nil {
		t.Fatal("invalid IP accepted")
	}
	if _, err := networkInterfaceNameForIP("127.0.0.1"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("loopback error = %v", err)
	}
}

func TestApplyDistributedNetworkInterfaceFillsTheAscendCollectiveInterface(t *testing.T) {
	ws := runtime.WorkloadSpec{Env: map[string]string{vllmHostIPEnv: "10.60.119.97"}}
	spec := proto.InstanceDesiredState{Engine: "vllm", NNodes: 4, HostNetwork: true}
	if err := applyDistributedNetworkInterface(&ws, spec, accelerator.VendorAscend, func(string) (string, error) {
		return "br_bond0_31", nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GLOO_SOCKET_IFNAME", "HCCL_SOCKET_IFNAME", "TP_SOCKET_IFNAME"} {
		if got := ws.Env[key]; got != "br_bond0_31" {
			t.Fatalf("%s = %q, want the interface that owns this node's address", key, got)
		}
	}
	if _, present := ws.Env["NCCL_SOCKET_IFNAME"]; present {
		t.Fatalf("NCCL was injected into an Ascend launch: %+v", ws.Env)
	}
}

func TestApplyDistributedNetworkInterfaceLeavesOtherVendorsCollectivesAlone(t *testing.T) {
	ws := runtime.WorkloadSpec{Env: map[string]string{
		vllmHostIPEnv:        "10.60.119.97",
		"GLOO_SOCKET_IFNAME": "private0",
		"NCCL_SOCKET_IFNAME": "private0",
	}}
	spec := proto.InstanceDesiredState{Engine: "vllm", NNodes: 2, HostNetwork: true}
	if err := applyDistributedNetworkInterface(&ws, spec, accelerator.VendorNVIDIA, func(string) (string, error) {
		t.Fatal("an NVIDIA launch whose own interfaces are set resolved one anyway")
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HCCL_SOCKET_IFNAME", "TP_SOCKET_IFNAME"} {
		if _, present := ws.Env[key]; present {
			t.Fatalf("%s was injected into a CUDA launch: %+v", key, ws.Env)
		}
	}
}

func TestSocketInterfaceEnvForUnknownVendorKeepsTheVLLMPair(t *testing.T) {
	got := socketInterfaceEnvFor("moonshot-npu")
	want := []string{"GLOO_SOCKET_IFNAME", "NCCL_SOCKET_IFNAME"}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}
