package deploy

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
)

// vllm-mooncake (upstream vLLM's MooncakeConnector on the Hygon image): the
// prefill instance starts a bootstrap server on its extra port, every
// instance hands peers its node address through vLLM's get_ip, and the
// transfer engine runs RDMA on a host RDMA network and TCP on any other.
func TestVLLMMooncakeLaunch(t *testing.T) {
	rdma := ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "eth0", HCAs: []string{"mlx5_0:1"}}
	for _, c := range []struct {
		name, role string
		network    ComputeNetwork
		config     string
		bootstrap  string
	}{
		{"prefill tcp", "prefill", ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "br0"},
			`{"kv_connector":"MooncakeConnector","kv_connector_extra_config":{"mooncake_protocol":"tcp"},"kv_load_failure_policy":"fail","kv_role":"kv_producer"}`, "30001"},
		{"decode tcp on one host", "decode", ComputeNetwork{},
			`{"kv_connector":"MooncakeConnector","kv_connector_extra_config":{"mooncake_protocol":"tcp"},"kv_load_failure_policy":"fail","kv_role":"kv_consumer"}`, ""},
		{"prefill rdma", "prefill", rdma,
			`{"kv_connector":"MooncakeConnector","kv_connector_extra_config":{"device_name":"mlx5_0","mooncake_protocol":"rdma"},"kv_load_failure_policy":"fail","kv_role":"kv_producer"}`, "30001"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := pdParams(c.role, LaunchConfig{}, c.network)
			p.Vendor = "hygon"
			p.PD.Mode = "vllm-mooncake"
			launch := BuildVLLM(p)
			if got := argValue(t, launch.Argv, "--kv-transfer-config"); got != c.config {
				t.Fatalf("config = %s", got)
			}
			if launch.Env["VLLM_HOST_IP"] != "10.0.0.5" {
				t.Fatalf("VLLM_HOST_IP = %q", launch.Env["VLLM_HOST_IP"])
			}
			if got, ok := launch.Env["VLLM_MOONCAKE_BOOTSTRAP_PORT"]; got != c.bootstrap || ok != (c.bootstrap != "") {
				t.Fatalf("VLLM_MOONCAKE_BOOTSTRAP_PORT = %q (set %v)", got, ok)
			}
			for _, key := range []string{"VLLM_NIXL_SIDE_CHANNEL_HOST", "VLLM_NIXL_SIDE_CHANNEL_PORT", "UCX_TLS", "VLLM_SSM_CONV_STATE_LAYOUT"} {
				if _, ok := launch.Env[key]; ok {
					t.Fatalf("Mooncake launch carries %s", key)
				}
			}
		})
	}
	// KV offload on the prefill role composes with the connector, which keeps
	// its protocol inside the MultiConnector.
	p := pdParams("prefill", LaunchConfig{KVOffloadingSize: 8}, ComputeNetwork{})
	p.PD.Mode = "vllm-mooncake"
	var config struct {
		Connector string `json:"kv_connector"`
		Extra     struct {
			Connectors []map[string]any `json:"connectors"`
		} `json:"kv_connector_extra_config"`
	}
	if err := json.Unmarshal([]byte(argValue(t, BuildVLLM(p).Argv, "--kv-transfer-config")), &config); err != nil {
		t.Fatal(err)
	}
	if config.Connector != "MultiConnector" || len(config.Extra.Connectors) != 2 || config.Extra.Connectors[0]["kv_connector"] != "MooncakeConnector" {
		t.Fatalf("config = %+v", config)
	}
	if extra, _ := config.Extra.Connectors[0]["kv_connector_extra_config"].(map[string]any); extra["mooncake_protocol"] != "tcp" {
		t.Fatalf("connector = %v", config.Extra.Connectors[0])
	}
}

func TestMooncakeProtocol(t *testing.T) {
	if MooncakeProtocol(ComputeNetwork{Type: netplan.TypeHostRDMA, HCAs: []string{"mlx5_0:1"}}) != "rdma" {
		t.Fatal("host RDMA")
	}
	for _, n := range []ComputeNetwork{{}, {Type: netplan.TypeHostEthernet, Interface: "eth0"}} {
		if MooncakeProtocol(n) != "tcp" {
			t.Fatalf("%+v", n)
		}
	}
}
