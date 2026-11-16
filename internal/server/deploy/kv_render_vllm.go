// kv_render_vllm.go renders the parts of vLLM's --kv-transfer-config: the PD
// connector of a PD instance's mode and role, and each KV offload backend's
// connector object (ADR 0069). Composing them into the one flag is
// kv_compose.go's job; nothing here decides how parts combine.

package deploy

import (
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/kvbackend"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// vllmOffloadRenderer renders one KV offload backend's contribution for a
// launch configuration, nil when the configuration asks for no offload.
type vllmOffloadRenderer func(q LaunchConfig) *kvContribution

// vllmOffloadRenderers are the renderers the product ships, keyed by
// kvbackend id. A backend row without a renderer here cannot be selected
// (TestKVBackendSetRendersEverySelectableRow).
func vllmOffloadRenderers() map[string]vllmOffloadRenderer {
	return map[string]vllmOffloadRenderer{kvbackend.Native: nativeVLLMOffload}
}

// nativeVLLMOffload is vLLM's own offload: the disk tier as a tiering
// OffloadingConnector, or the CPU tier, which without a PD connector is
// carried by vLLM's own flags and only joins a composition as an object.
func nativeVLLMOffload(q LaunchConfig) *kvContribution {
	switch {
	case q.KVDiskOffload:
		return &kvContribution{object: diskKVTransferObject(q)}
	case q.KVOffloadingSize > 0:
		return &kvContribution{object: map[string]any{
			"kv_connector": "OffloadingConnector", "kv_role": "kv_both",
			"kv_connector_extra_config": map[string]any{"cpu_bytes_to_use": KVCPUTierBytes(q)},
		}, standaloneAsFlags: true}
	}
	return nil
}

// vllmPDContribution is a PD instance's connector in its role's connector
// role, and the mode's KV load failure policy, which the engine reads only at
// the top level of the composed config.
func vllmPDContribution(p Params) (*kvContribution, string) {
	mode := p.PD.mode()
	policy := mode.EffectiveKVLoadFailure(p.PD.KVLoadFailure)
	role := mode.ConnectorRole(p.PD.Role)
	pd := map[string]any{"kv_connector": mode.Connector, "kv_role": role}
	if mode.ConnectorPortKey != "" && p.PD.SideChannelPort > 0 {
		pd[mode.ConnectorPortKey] = p.sideChannelPortText()
	}
	if mode.ConnectorParallelism {
		sides := map[string]any{}
		for _, r := range mode.Roles {
			sides[r] = map[string]any{"tp_size": max(p.PD.RoleTP[r], 1), "dp_size": 1}
		}
		pd["kv_connector_extra_config"] = sides
	}
	if len(mode.ConnectorExtra) > 0 || mode.Transport.MooncakeBootstrap() {
		extra, _ := pd["kv_connector_extra_config"].(map[string]any)
		if extra == nil {
			extra = map[string]any{}
		}
		for key, value := range mode.ConnectorExtra {
			extra[key] = value
		}
		// TransportMooncake follows the network the group shares; the RDMA
		// transport states its protocol in ConnectorExtra.
		if mode.Transport == pdmode.TransportMooncake {
			extra["mooncake_protocol"] = MooncakeProtocol(p.ComputeNetwork)
		}
		if hcas := hcaDevices(p.ComputeNetwork.HCAs); len(hcas) > 0 && extra["mooncake_protocol"] == "rdma" {
			extra["device_name"] = strings.Join(hcas, ",")
		}
		pd["kv_connector_extra_config"] = extra
	}
	// No engine_id: vLLM gives each engine process a fresh one, and the
	// gateway reads it from the prefill's bootstrap server
	// (pdmode.Mode.SynthesizedHandoff).
	return &kvContribution{object: pd, role: role}, policy
}

func diskKVTransferConfig(q LaunchConfig) string {
	config, _ := composeVLLMKVTransfer(nil, &kvContribution{object: diskKVTransferObject(q)}, "")
	return config
}

func diskKVTransferObject(q LaunchConfig) map[string]any {
	// The thread counts are folded into one composite flag, so an unset one has
	// no "emit nothing" spelling of its own: the dictionary's default is what
	// the connector is given.
	readThreads, writeThreads := q.KVOffloadReadThreads, q.KVOffloadWriteThreads
	if readThreads <= 0 {
		readThreads = defaultInt(EngineVLLM, "kv_offload_read_threads")
	}
	if writeThreads <= 0 {
		writeThreads = defaultInt(EngineVLLM, "kv_offload_write_threads")
	}
	extra := map[string]any{
		"spec_name":        "TieringOffloadingSpec",
		"cpu_bytes_to_use": KVCPUTierBytes(q),
		"secondary_tiers": []map[string]any{{
			"type": "fs", "root_dir": filepath.Clean(q.SharedKVDir),
			"n_read_threads": readThreads, "n_write_threads": writeThreads,
		}},
	}
	if q.KVOffloadBlockSize > 0 {
		extra["block_size"] = q.KVOffloadBlockSize
	}
	return map[string]any{
		"kv_connector": "OffloadingConnector", "kv_role": "kv_both",
		"kv_connector_extra_config": extra,
	}
}
