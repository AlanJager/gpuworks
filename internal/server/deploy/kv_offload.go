// kv_offload.go holds the rules a KV offload backend selection (ADR 0069) is
// held to: the id names a backend of the engine, a backend that replaces the
// engine's own tiers is not combined with them, no argument layer overrides
// the connector it renders, and the target and topology are ones it runs on.
// Every refusal is a code (package messages), never prose.

package deploy

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/kvbackend"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// validateKVBackend checks the selection against the engine's catalog. A
// stored row whose id the catalog no longer has still decodes; it is refused
// here, on evaluate, edit and launch alike.
func validateKVBackend(engine string, q LaunchConfig) error {
	if q.KVOffloadBackend == "" {
		return nil
	}
	row, ok := selectedKVBackend(engine, q)
	if !ok || row.Default {
		// The default row is selected by stating nothing; naming it is not a
		// selection the dictionary offers.
		return messages.NewProductError(messages.ErrorKVOffloadBackendUnknown,
			fmt.Sprintf("%s has no KV offload backend %q", engine, q.KVOffloadBackend),
			map[string]any{"key": paramKVOffloadBackend, "engine": engine, "backend": q.KVOffloadBackend})
	}
	for _, key := range row.Replaces {
		if nativeKVValueEffective(q, key) {
			return messages.NewProductError(messages.ErrorKVOffloadBackendConflictsNativeTier,
				fmt.Sprintf("KV offload backend %s replaces %s", row.ID, key),
				map[string]any{"key": key, "backend": row.ID})
		}
	}
	return nil
}

// nativeKVValueEffective reports whether q states a native KV offload key in
// a way that changes the launch. A value the platform states by default (a
// disk tier of false, thread counts of a disk tier that is off) does not, so
// switching a stored native row to a backend is refused only for what the
// row actually runs with. The directory and its ceiling count on their own:
// they give the instance a managed disk mount (kvDiskMount).
func nativeKVValueEffective(q LaunchConfig, key string) bool {
	switch key {
	case paramKVOffloadingSize:
		return q.KVOffloadingSize > 0
	case "kv_disk_offload":
		return q.KVDiskOffload
	case "shared_kv_dir":
		return strings.TrimSpace(q.SharedKVDir) != ""
	case "kv_disk_limit_gib":
		return q.KVDiskLimitGiB > 0
	case "kv_offload_read_threads":
		return q.KVDiskOffload && q.KVOffloadReadThreads > 0
	case "kv_offload_write_threads":
		return q.KVDiskOffload && q.KVOffloadWriteThreads > 0
	}
	// A key this switch does not know is judged as stated: a backend that
	// names it in Replaces must not be combined with it at all.
	values, err := launchConfigValues(q)
	return err == nil && dependencyValueEnabled(values[key])
}

// validateKVBackendLaunch is the selection's check on a concrete launch. It
// repeats validateKVBackend, because a stored row reaches a launch without a
// new evaluation, and refuses an argument layer that states a flag which
// would replace the backend's connector. Under PD the PD rule
// (PD_CONNECTOR_ARG_CONFLICT) already refuses those flags in every layer.
func validateKVBackendLaunch(engine Engine, p Params) error {
	if err := validateKVBackend(engine.Name(), p.Quick); err != nil {
		return err
	}
	row, ok := selectedKVBackend(engine.Name(), p.Quick)
	if !ok || !row.External || p.PD != nil {
		return nil
	}
	flags := hooks(engine).KVConflictFlags()
	for _, layer := range [][]string{p.TemplateArgs, p.ExtraArgs} {
		if matched := matchConflictFlags(layer, flags); len(matched) > 0 {
			return messages.NewProductError(messages.ErrorKVOffloadBackendConflictsArg,
				fmt.Sprintf("%s would replace the KV offload backend %s's connector", matched[0], row.ID),
				map[string]any{"key": ParamFieldExtraArgs, "backend": row.ID, "flag": matched[0]})
		}
	}
	return nil
}

// matchConflictFlags returns the flags (as listed) that tokens state in any
// spelling the engine's argument parser accepts: --flag value, --flag=value,
// underscores for hyphens, and a quoted token.
func matchConflictFlags(tokens, flags []string) []string {
	var out []string
	for _, flag := range flags {
		want := normalizeFlagSpelling(flag)
		for _, token := range tokens {
			name, _, _ := strings.Cut(unquoteArgument(token), "=")
			if strings.HasPrefix(name, "--") && normalizeFlagSpelling(name) == want {
				out = append(out, flag)
				break
			}
		}
	}
	return out
}

func normalizeFlagSpelling(flag string) string {
	return "--" + strings.ReplaceAll(strings.TrimPrefix(flag, "--"), "_", "-")
}

// validateKVBackendTarget refuses a backend on a target or in a topology it
// does not run in. ValidateTargetFeatures calls it, so both validating modes
// reach it; CollectTargetCapabilities does not, because planning validates
// the assigned target afterwards.
func validateKVBackendTarget(engine string, f requestedTopology, p Params) error {
	if p.Quick.KVOffloadBackend == "" {
		return nil
	}
	row, ok := selectedKVBackend(engine, p.Quick)
	if !ok {
		return nil // validateKVBackend refuses an unknown id
	}
	if !row.Supports(p.Vendor) {
		return messages.NewProductError(messages.ErrorKVOffloadBackendUnsupportedOnTarget,
			fmt.Sprintf("KV offload backend %s does not run on %s", row.ID, accelerator.VendorBrand(p.Vendor)),
			map[string]any{"backend": row.ID, "vendor": accelerator.VendorBrand(p.Vendor)})
	}
	if !row.TopologyAllows(f.nodes, f.pp, f.dp) {
		return messages.NewProductError(messages.ErrorKVOffloadBackendTopologyUnsupported,
			fmt.Sprintf("KV offload backend %s does not run on %d nodes (pipeline %v, data %v)", row.ID, f.nodes, f.pp, f.dp),
			map[string]any{"backend": row.ID, "nnodes": f.nodes, "pp": f.pp, "dp": f.dp})
	}
	return nil
}

// requestedTopology is what a resolved launch asks for in the terms a backend
// bounds: how many nodes, and whether pipeline or data parallelism runs.
type requestedTopology struct {
	nodes  int
	pp, dp bool
}

// kvBackendRequestsExternalCache reports whether the launch's selection keeps
// the KV cache outside the engine (RequestedFeatures.ExternalKVCache).
func kvBackendRequestsExternalCache(engine string, q LaunchConfig) bool {
	if q.KVOffloadBackend == "" {
		return false
	}
	row, ok := selectedKVBackend(engine, q)
	return ok && row.External
}

// kvBackendKeptOn applies a backend's PD role scope: a role outside the
// selected row's PDRoles launches with no KV offload at all. An engine with
// no rows (SGLang) keeps the original rule, prefill only.
func kvBackendKeptOn(engine string, q LaunchConfig, role string) bool {
	row, ok := selectedKVBackend(engine, q)
	if !ok {
		row = kvbackend.Backend{PDRoles: []string{pdmode.RolePrefill}}
	}
	return row.KeptOn(role)
}

// launchConfigValues reads q by its dictionary keys.
func launchConfigValues(q LaunchConfig) (map[string]any, error) {
	encoded, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	return values, json.Unmarshal(encoded, &values)
}
