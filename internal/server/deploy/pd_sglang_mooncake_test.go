package deploy

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
)

// sglang-mooncake (SGLang with Mooncake RDMA on Hygon DCU), in the layout a
// hand-written GLM-5.3 launch uses: a prefill instance of two nodes at TP 8 x
// PP 2 and a decode instance of two nodes at TP 16.
func TestSGLangHygonMooncakeLaunch(t *testing.T) {
	engine, _ := SelectEngine("sglang")
	network := ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond1", HCAs: []string{"mlx5_2:1", "mlx5_3:1", "mlx5_2:2"}}
	for _, c := range []struct {
		role   string
		tp, pp int
		rank   int
	}{
		{role: "prefill", tp: 8, pp: 2, rank: 1},
		{role: "decode", tp: 16, pp: 1, rank: 0},
	} {
		p := Params{ModelPath: "/model", APIPort: 30000, TensorParallel: c.tp, PipelineParallel: c.pp, Vendor: "hygon",
			NNodes: 2, NodeRank: c.rank, MasterAddr: "10.113.46.21", MasterPort: 5001, NodeAddress: "10.113.46.22", ComputeNetwork: network,
			PD: &PDLaunch{Mode: "sglang-mooncake", Role: c.role, SideChannelHost: "10.113.46.22", SideChannelPort: 8199}}
		resolved, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: p})
		if err != nil {
			t.Fatal(err)
		}
		argv := append(append([]string(nil), resolved.Entrypoint...), resolved.Argv...)
		if argValue(t, argv, "--disaggregation-mode") != c.role || argValue(t, argv, "--disaggregation-transfer-backend") != "mooncake" ||
			argValue(t, argv, "--disaggregation-ib-device") != "mlx5_2,mlx5_3" {
			t.Fatalf("%s argv = %v", c.role, argv)
		}
		// Only the prefill serves a bootstrap; every one of its ranks on the
		// port the controller gave the unit.
		if bootstrap := countFlag(argv, "--disaggregation-bootstrap-port"); (c.role == "prefill") != (bootstrap == 1) {
			t.Fatalf("%s argv = %v", c.role, argv)
		}
		if c.role == "prefill" && argValue(t, argv, "--disaggregation-bootstrap-port") != "8199" {
			t.Fatalf("prefill argv = %v", argv)
		}
		for key, want := range map[string]string{
			"NCCL_IB_HCA":        "mlx5_2:1,mlx5_3:1,mlx5_2:2",
			"NCCL_SOCKET_IFNAME": "bond1",
			"SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION": "0",
		} {
			if resolved.Env[key] != want {
				t.Fatalf("%s env %s = %q, want %q (env %v)", c.role, key, resolved.Env[key], want, resolved.Env)
			}
		}
		// What a Hygon launch further needs (ROCSHMEM's devices and
		// bootstrap interface, Mooncake's device filter, the address SGLang
		// registers with the bootstrap) a template derives from the generic
		// variables above; the platform states none of it.
		for _, key := range []string{"ROCSHMEM_ALLOWED_IBV_DEVICES", "ROCSHMEM_BOOTSTRAP_SOCKET_IFNAME", "MC_TE_FILTERS", "SGLANG_HOST_IP"} {
			if _, set := resolved.Env[key]; set {
				t.Fatalf("%s env states %s: %v", c.role, key, resolved.Env)
			}
		}
		if !hasDockerArgValue(resolved.DockerArgs, "--device", "/dev/infiniband") {
			t.Fatalf("%s docker args = %v", c.role, resolved.DockerArgs)
		}
	}
}
