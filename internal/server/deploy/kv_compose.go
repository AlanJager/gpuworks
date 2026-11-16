// kv_compose.go is the single assembly point of vLLM's --kv-transfer-config
// (ADR 0069). A launch contributes at most one PD connector and at most one
// KV offload backend; the flag carries both, because a second flag would
// replace the first: measured on vLLM 0.25.1, --kv-offloading-size next to a
// NIXL config silently drops NIXL.

package deploy

import "encoding/json"

// kvContribution is one part of the composed config.
type kvContribution struct {
	object map[string]any
	// role is the PD connector's role, which becomes the outer role when the
	// PD part is wrapped together with an offload part.
	role string
	// standaloneAsFlags: an offload part that, with no PD part beside it, is
	// carried by the engine's own flags instead of a connector object (vLLM's
	// native CPU tier: --kv-offloading-size and --kv-offloading-backend).
	standaloneAsFlags bool
}

// composeVLLMKVTransfer answers the --kv-transfer-config value, or false when
// the launch states none:
//
//   - no part, or only an offload part carried by flags: no config;
//   - one offload part: that object;
//   - only a PD part: that object;
//   - both: a MultiConnector of [pd, offload] in the PD role.
//
// The KV load failure policy is a PD setting the engine reads only at the top
// level, so it is set on the outermost object.
func composeVLLMKVTransfer(pd, offload *kvContribution, policy string) (string, bool) {
	var config map[string]any
	switch {
	case pd == nil && (offload == nil || offload.standaloneAsFlags):
		return "", false
	case pd == nil:
		config = cloneJSONObject(offload.object)
	case offload == nil:
		config = cloneJSONObject(pd.object)
	default:
		config = map[string]any{
			"kv_connector": "MultiConnector", "kv_role": pd.role,
			"kv_connector_extra_config": map[string]any{"connectors": []map[string]any{pd.object, offload.object}},
		}
	}
	if pd != nil && policy != "" {
		config["kv_load_failure_policy"] = policy
	}
	raw, _ := json.Marshal(config)
	return string(raw), true
}

// cloneJSONObject copies the top level, which is all the composer writes.
func cloneJSONObject(object map[string]any) map[string]any {
	out := make(map[string]any, len(object)+1)
	for key, value := range object {
		out[key] = value
	}
	return out
}

// vllmKVTransferConfig composes this launch's parts: the PD connector of a PD
// instance, and the selected KV offload backend (PDRoleLaunchConfig has
// already dropped it on a role that does not keep it).
func vllmKVTransferConfig(p Params) (string, bool) {
	var pd *kvContribution
	policy := ""
	if p.PD != nil {
		pd, policy = vllmPDContribution(p)
	}
	return composeVLLMKVTransfer(pd, nativeVLLMOffload(p.Quick), policy)
}
