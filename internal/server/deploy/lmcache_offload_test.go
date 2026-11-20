package deploy

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/kvbackend"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

func parsedKVTransferConfig(t *testing.T, p Params) (map[string]any, []string) {
	t.Helper()
	launch := BuildVLLM(p)
	raw := argValue(t, launch.Argv, "--kv-transfer-config")
	if raw == "" {
		t.Fatalf("missing --kv-transfer-config: %v", launch.Argv)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	return config, launch.Argv
}

func connectorChildren(t *testing.T, config map[string]any) []map[string]any {
	t.Helper()
	extra, _ := config["kv_connector_extra_config"].(map[string]any)
	children, _ := extra["connectors"].([]any)
	if len(children) == 0 {
		t.Fatalf("no child connectors: %+v", config)
	}
	out := make([]map[string]any, 0, len(children))
	for _, child := range children {
		m, _ := child.(map[string]any)
		out = append(out, m)
	}
	return out
}

func TestLMCacheBackendPDAndNonPDShapes(t *testing.T) {
	q := LaunchConfig{KVOffloadBackend: kvbackend.LMCache}
	prefillParams := pdParams(pdmode.RolePrefill, q, ComputeNetwork{})
	prefill, argv := parsedKVTransferConfig(t, prefillParams)
	if prefill["kv_connector"] != "MultiConnector" || prefill["kv_role"] != "kv_producer" {
		t.Fatalf("outer config = %+v", prefill)
	}
	if prefill["kv_connector_module_path"] != vllmMultiConnectorModule {
		t.Fatalf("outer module path = %v", prefill["kv_connector_module_path"])
	}
	children := connectorChildren(t, prefill)
	if len(children) != 2 || children[0]["kv_connector"] != "NixlConnector" {
		t.Fatalf("children = %+v", children)
	}
	lm := children[1]
	if lm["kv_connector"] != "LMCacheMPConnector" || lm["kv_role"] != "kv_producer" {
		t.Fatalf("LMCache child = %+v", lm)
	}
	if lm["kv_connector_module_path"] != lmcacheMPConnectorModule {
		t.Fatalf("module path = %v", lm["kv_connector_module_path"])
	}
	extra, _ := lm["kv_connector_extra_config"].(map[string]any)
	if extra["lmcache.mp.host"] != lmcacheDefaultMPHost || extra["lmcache.mp.port"] != float64(lmcacheDefaultMPPort) || extra["lmcache.mp.mp_transfer_mode"] != lmcacheDefaultTransferMode {
		t.Fatalf("LMCache extra = %+v", extra)
	}
	if prefill["kv_load_failure_policy"] != "fail" {
		t.Fatalf("policy = %v", prefill["kv_load_failure_policy"])
	}
	if countFlag(argv, "--kv-offloading-size") != 0 || countFlag(argv, "--kv-offloading-backend") != 0 {
		t.Fatalf("native vLLM offload flags leaked: %v", argv)
	}

	decodeQuick := PDRoleLaunchConfig(EngineVLLM, q, pdmode.RoleDecode)
	if decodeQuick.KVOffloadBackend != "" {
		t.Fatalf("decode retained backend %q", decodeQuick.KVOffloadBackend)
	}
	decode, decodeArgv := parsedKVTransferConfig(t, pdParams(pdmode.RoleDecode, decodeQuick, ComputeNetwork{}))
	if decode["kv_connector"] != "NixlConnector" || decode["kv_role"] != "kv_consumer" {
		t.Fatalf("decode = %+v", decode)
	}
	if countFlag(decodeArgv, "--kv-offloading-size") != 0 {
		t.Fatalf("decode has a native tier: %v", decodeArgv)
	}

	plain, _ := parsedKVTransferConfig(t, Params{ModelPath: "/model", APIPort: 30000, Quick: q})
	if plain["kv_connector"] != "LMCacheMPConnector" || plain["kv_role"] != "kv_both" {
		t.Fatalf("non-PD = %+v", plain)
	}
	if plain["kv_connector_module_path"] != lmcacheMPConnectorModule {
		t.Fatalf("non-PD module path = %v", plain["kv_connector_module_path"])
	}
}

func TestLMCacheBackendPreservesMooncakeTransportModes(t *testing.T) {
	q := LaunchConfig{KVOffloadBackend: kvbackend.LMCache}
	t.Run("ascend mooncake hccs", func(t *testing.T) {
		p := pdParams(pdmode.RolePrefill, q, ComputeNetwork{Type: netplan.TypeAcceleratorHCCS, Interface: "eth0"})
		p.Vendor = accelerator.VendorAscend
		p.PD.Mode = "vllm-ascend-mooncake"
		p.PD.RoleTP = map[string]int{pdmode.RolePrefill: 16, pdmode.RoleDecode: 16}
		cfg, _ := parsedKVTransferConfig(t, p)
		children := connectorChildren(t, cfg)
		if children[0]["kv_connector"] != "MooncakeConnectorV1" || children[0]["kv_port"] != "30001" {
			t.Fatalf("ascend mooncake child = %+v", children[0])
		}
		if children[1]["kv_connector"] != "LMCacheMPConnector" || cfg["kv_connector_module_path"] != vllmMultiConnectorModule {
			t.Fatalf("LMCache composition = %+v", cfg)
		}
	})
	t.Run("mooncake rdma hca and tcp", func(t *testing.T) {
		rdma := pdParams(pdmode.RolePrefill, q, ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "eth0", HCAs: []string{"mlx5_0:1"}})
		rdma.Vendor = accelerator.VendorNVIDIA
		rdma.PD.Mode = "vllm-mooncake"
		rdmaLaunch := BuildVLLM(rdma)
		if rdmaLaunch.Env["VLLM_MOONCAKE_BOOTSTRAP_PORT"] != "30001" {
			t.Fatalf("missing bootstrap port: %v", rdmaLaunch.Env)
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(argValue(t, rdmaLaunch.Argv, "--kv-transfer-config")), &cfg); err != nil {
			t.Fatal(err)
		}
		mcExtra, _ := connectorChildren(t, cfg)[0]["kv_connector_extra_config"].(map[string]any)
		if mcExtra["mooncake_protocol"] != "rdma" || mcExtra["device_name"] != "mlx5_0" {
			t.Fatalf("rdma mooncake extra = %+v", mcExtra)
		}
		tcp := pdParams(pdmode.RolePrefill, q, ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "eth0"})
		tcp.Vendor = accelerator.VendorNVIDIA
		tcp.PD.Mode = "vllm-mooncake"
		tcpCfg, _ := parsedKVTransferConfig(t, tcp)
		tcpExtra, _ := connectorChildren(t, tcpCfg)[0]["kv_connector_extra_config"].(map[string]any)
		if tcpExtra["mooncake_protocol"] != "tcp" || tcpExtra["device_name"] != nil {
			t.Fatalf("tcp mooncake extra = %+v", tcpExtra)
		}
	})
}

func TestLMCacheBackendVendorTopologyAndConflictGates(t *testing.T) {
	q := LaunchConfig{KVOffloadBackend: kvbackend.LMCache}
	for _, vendor := range []string{accelerator.VendorNVIDIA, accelerator.VendorAscend, ""} {
		launch, err := resolveKVTest(kvTestParams(q, vendor), ValidateAssignedTarget)
		if err != nil {
			t.Fatalf("vendor %q rejected: %v", vendor, err)
		}
		if launch.RequestedFeatures.PD || !launch.RequestedFeatures.ExternalKVCache {
			t.Fatalf("vendor %q features = %+v", vendor, launch.RequestedFeatures)
		}
		if want := (vLLMEngine{}).Profile().ShmBytes; launch.ShmBytes != want {
			t.Fatalf("vendor %q allocated native shm: got %d want %d", vendor, launch.ShmBytes, want)
		}
		if len(launch.Mounts) != 0 {
			t.Fatalf("vendor %q produced disk mounts: %+v", vendor, launch.Mounts)
		}
	}
	_, err := resolveKVTest(kvTestParams(q, accelerator.VendorKunlunxin), ValidateAssignedTarget)
	wantCode(t, err, messages.ErrorKVOffloadBackendUnsupportedOnTarget)

	multiNode := kvTestParams(q, accelerator.VendorNVIDIA)
	multiNode.NNodes = 2
	multiNode.PipelineParallel = 2
	_, err = resolveKVTest(multiNode, ValidateAssignedTarget)
	wantCode(t, err, messages.ErrorKVOffloadBackendTopologyUnsupported)

	for name, conflict := range map[string]LaunchConfig{
		"size":       {KVOffloadBackend: kvbackend.LMCache, KVOffloadingSize: 1},
		"disk":       {KVOffloadBackend: kvbackend.LMCache, KVDiskOffload: true, SharedKVDir: "/kv"},
		"shared dir": {KVOffloadBackend: kvbackend.LMCache, SharedKVDir: "/kv"},
		"disk limit": {KVOffloadBackend: kvbackend.LMCache, KVDiskLimitGiB: 1},
		"block size": {KVOffloadBackend: kvbackend.LMCache, KVOffloadBlockSize: 128},
	} {
		t.Run(name, func(t *testing.T) {
			wantCode(t, ValidateLaunchConfigForEngine(EngineVLLM, conflict), messages.ErrorKVOffloadBackendConflictsNativeTier)
		})
	}
}

func TestLMCacheBackendTemplateExportAndRoundTrip(t *testing.T) {
	q := LaunchConfig{KVOffloadBackend: kvbackend.LMCache}
	exported, err := ExportLaunchArgs(EngineVLLM, q, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(exported.Dropped, paramKVOffloadBackend) {
		t.Fatalf("selector dropped on export: %+v", exported)
	}
	params, residual, err := ReverseArgs(EngineVLLM, exported.Args)
	if err != nil || params[paramKVOffloadBackend] != kvbackend.LMCache || len(residual) != 0 {
		t.Fatalf("reverse = %v, residual = %v, err = %v", params, residual, err)
	}
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, TemplateArgs: exported.Args})
	if err != nil || out.Quick.KVOffloadBackend != kvbackend.LMCache || len(out.TemplateResidual) != 0 {
		t.Fatalf("materialize = %+v, err = %v", out, err)
	}
}

func TestLMCacheBackendPDManualCommandDraftRoundTrip(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	stubs := engineStubs(t, "vllm")
	q := LaunchConfig{KVOffloadBackend: kvbackend.LMCache}
	net := ComputeNetwork{Type: netplan.TypeAcceleratorHCCS, Interface: "eth0"}

	prefill := pdParams(pdmode.RolePrefill, PDRoleLaunchConfig(EngineVLLM, q, pdmode.RolePrefill), net)
	prefill.Vendor = accelerator.VendorAscend
	prefill.APIPort = 30000
	prefill.PD.Mode = "vllm-ascend-mooncake"
	prefill.PD.SideChannelPort = 30011
	prefill.PD.RoleTP = map[string]int{pdmode.RolePrefill: 16, pdmode.RoleDecode: 16}

	decode := pdParams(pdmode.RoleDecode, PDRoleLaunchConfig(EngineVLLM, q, pdmode.RoleDecode), net)
	decode.Vendor = accelerator.VendorAscend
	decode.APIPort = 30010
	decode.PD.Mode = "vllm-ascend-mooncake"
	decode.PD.SideChannelPort = 30022
	decode.PD.RoleTP = map[string]int{pdmode.RolePrefill: 16, pdmode.RoleDecode: 16}

	ranks := []ResolveLaunchInput{
		{Engine: vLLMEngine{}, Params: prefill},
		{Engine: vLLMEngine{}, Params: decode},
	}
	draft, ok, err := ManualCommandDraft(ranks)
	if err != nil || !ok {
		t.Fatalf("ManualCommandDraft ok=%v err=%v", ok, err)
	}
	for _, input := range ranks {
		engineLaunch, err := ResolveLaunch(input)
		if err != nil {
			t.Fatalf("engine ResolveLaunch(%s): %v", input.Params.PD.Role, err)
		}
		manual := input
		manual.ContainerOverrides = ContainerOverrides{CommandMode: string(CommandManual), CmdDisplay: draft}
		manualLaunch, err := ResolveLaunch(manual)
		if err != nil {
			t.Fatalf("manual ResolveLaunch(%s): %v", input.Params.PD.Role, err)
		}
		got, err := executedArgv(t, stubs, manualLaunch)
		if err != nil {
			t.Fatalf("executedArgv(%s): %v", input.Params.PD.Role, err)
		}
		if want := launchArguments(engineLaunch); !slices.Equal(got, want) {
			t.Fatalf("role %s manual argv =\n  %q\nwant\n  %q", input.Params.PD.Role, got, want)
		}
	}
}
