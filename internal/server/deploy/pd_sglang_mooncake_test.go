package deploy

import (
	"strings"
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

// Every rank of a multi-node prefill can name the leader's bootstrap port
// (${GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT}) instead of its own, so a build
// that does not align its ranks' ports itself still registers with the
// leader; ${GPUWORKS_PD_SIDE_CHANNEL_PORT} stays the rank's own.
func TestManualCommandNamesTheLeadersSideChannel(t *testing.T) {
	engine, _ := SelectEngine("sglang")
	script := `exec python3 -m sglang.launch_server --disaggregation-bootstrap-port ${GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT} --own ${GPUWORKS_PD_SIDE_CHANNEL_PORT}`
	for _, c := range []struct {
		rank, own int
		address   string
		want      string
	}{
		{rank: 0, own: 31104, address: "10.113.46.21", want: "--disaggregation-bootstrap-port 31104 --own 31104"},
		{rank: 1, own: 31107, address: "10.113.46.22", want: "--disaggregation-bootstrap-port 31104 --own 31107"},
	} {
		p := Params{ModelPath: "/model", TensorParallel: 8, NNodes: 2, NodeRank: c.rank, Headless: c.rank > 0, MasterAddr: "10.113.46.21", MasterPort: 31105,
			NodeAddress: c.address,
			PD:          &PDLaunch{Mode: "sglang-mooncake", Role: "prefill", SideChannelPort: c.own, LeaderSideChannelPort: 31104}}
		launch, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: p,
			ContainerOverrides: ContainerOverrides{CommandMode: string(CommandManual), CmdDisplay: script}})
		if err != nil {
			t.Fatal(err)
		}
		if got := launch.Argv[len(launch.Argv)-1]; !strings.HasSuffix(got, c.want) {
			t.Fatalf("rank %d script = %q, want suffix %q", c.rank, got, c.want)
		}
	}
}

// The command the platform composes points a prefill follower at the
// leader's bootstrap server: the leader's port, not the follower's own,
// whatever port broadcast the SGLang build has. The leader and a decoder are
// unchanged.
func TestSGLangPrefillFollowerNamesTheLeadersBootstrapPort(t *testing.T) {
	engine, _ := SelectEngine("sglang")
	network := ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "eth0", HCAs: []string{"mlx5_0:1"}}
	for _, c := range []struct {
		role      string
		rank, own int
		want      string
	}{
		{role: "prefill", rank: 0, own: 31104, want: "31104"},
		{role: "prefill", rank: 1, own: 31107, want: "31104"},
		{role: "decode", rank: 1, own: 0, want: ""},
	} {
		p := Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 2, Vendor: "nvidia", NNodes: 2, NodeRank: c.rank, Headless: c.rank > 0,
			MasterAddr: "10.0.0.1", MasterPort: 31105, NodeAddress: "10.0.0.2", ComputeNetwork: network,
			PD: &PDLaunch{Mode: "sglang-nixl", Role: c.role, SideChannelHost: "10.0.0.2", SideChannelPort: c.own, LeaderSideChannelPort: 31104}}
		resolved, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: p})
		if err != nil {
			t.Fatal(err)
		}
		argv := append(append([]string(nil), resolved.Entrypoint...), resolved.Argv...)
		got := ""
		if countFlag(argv, "--disaggregation-bootstrap-port") == 1 {
			got = argValue(t, argv, "--disaggregation-bootstrap-port")
		}
		if got != c.want {
			t.Fatalf("%s rank %d bootstrap port = %q, want %q (argv %v)", c.role, c.rank, got, c.want, argv)
		}
	}
	// A preview names the variable rather than a number.
	p := Params{ModelPath: "/model", TensorParallel: 2, NNodes: 2, NodeRank: 1, Headless: true, Symbolic: true,
		PD: &PDLaunch{Mode: "sglang-nixl", Role: "prefill", SideChannelPort: 31107, LeaderSideChannelPort: 31104}}
	if got := p.sglangBootstrapPortText(); got != "${GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT}" {
		t.Fatalf("preview bootstrap port = %q", got)
	}
}
