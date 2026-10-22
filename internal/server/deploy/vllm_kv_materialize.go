package deploy

import (
	"encoding/json"
	"strings"
)

// canUnsetTemplateFlag recognizes only the disk-tier transfer object emitted
// by this vLLM adapter. --kv-transfer-config also carries PD and third-party
// connectors; those stay opaque and produce an explicit unset conflict.
func (vLLMEngine) CanUnsetTemplateFlag(flag, raw string) bool {
	if flag != "--kv-transfer-config" {
		return false
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(unquoteArgument(raw)), &config); err != nil {
		return false
	}
	if !onlyVLLMKVKeys(config, "kv_connector", "kv_role", "kv_connector_extra_config") || config["kv_connector"] != "OffloadingConnector" || config["kv_role"] != "kv_both" {
		return false
	}
	extra, ok := config["kv_connector_extra_config"].(map[string]any)
	if !ok || !onlyVLLMKVKeys(extra, "spec_name", "cpu_bytes_to_use", "secondary_tiers", "block_size") || extra["spec_name"] != "TieringOffloadingSpec" || !positiveJSONNumber(extra["cpu_bytes_to_use"]) {
		return false
	}
	tiers, ok := extra["secondary_tiers"].([]any)
	if !ok || len(tiers) != 1 {
		return false
	}
	tier, ok := tiers[0].(map[string]any)
	if !ok || !onlyVLLMKVKeys(tier, "type", "root_dir", "n_read_threads", "n_write_threads") || tier["type"] != "fs" {
		return false
	}
	root, rootOK := tier["root_dir"].(string)
	return rootOK && strings.TrimSpace(root) != "" && positiveJSONNumber(tier["n_read_threads"]) && positiveJSONNumber(tier["n_write_threads"])
}

func positiveJSONNumber(value any) bool {
	number, ok := value.(float64)
	return ok && number > 0
}

func onlyVLLMKVKeys(values map[string]any, allowed ...string) bool {
	known := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		known[key] = struct{}{}
	}
	for key := range values {
		if _, ok := known[key]; !ok {
			return false
		}
	}
	return true
}
