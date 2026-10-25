package deploy

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const maxResolvedPreStartScriptBytes = 64 << 10

// rejectUnrunnableLaunch refuses a merged launch that is certain to fail and
// that the user cannot make work as written: the compatibility script and a
// preparation hook that cannot be composed with it, or a preparation script the
// agent will not accept. Everything the platform merely cannot prove is a
// warning (see launchWarnings). preCompatScript is the preparation present
// before the platform added its own compatibility script.
func rejectUnrunnableLaunch(launch ResolvedLaunch, image string, compat ascendRuntimeCompat, preCompatScript string) error {
	if compat != ascendRuntimeCompatNone && len(launch.PreStartArgv) > 0 {
		if compat == ascendRuntimeCompatV1 {
			return messages.NewProductError(messages.ErrorAscendV1CompatPreStart,
				"the vLLM-Ascend rc2 A3 V1 compatibility script cannot be combined with pre_start_argv; remove the preparation hook or use Docker or nerdctl",
				map[string]any{"field": "pre_start_argv", "image": image})
		}
		return messages.NewProductError(messages.ErrorAscendCompatPreStartArgv,
			"the selected Ascend compatibility script cannot be combined with pre_start_argv; use pre_start_script alone, remove the hook, or use Docker or nerdctl",
			map[string]any{"field": "pre_start_argv", "image": image})
	}
	if compat == ascendRuntimeCompatV1 && strings.TrimSpace(preCompatScript) != "" {
		return messages.NewProductError(messages.ErrorAscendV1CompatPreStart,
			"the vLLM-Ascend rc2 A3 V1 compatibility script cannot be combined with pre_start_script; remove the preparation hook or use the V2 model runner, Docker or nerdctl",
			map[string]any{"field": "pre_start_script", "image": image})
	}
	if len(launch.PreStartScript) > maxResolvedPreStartScriptBytes {
		return messages.NewProductError(messages.ErrorPreStartScriptTooLarge,
			fmt.Sprintf("pre_start_script is %d bytes, above the agent's %d-byte limit", len(launch.PreStartScript), maxResolvedPreStartScriptBytes),
			map[string]any{"bytes": len(launch.PreStartScript), "max_bytes": maxResolvedPreStartScriptBytes})
	}
	return nil
}

// launchWarnings lists what the platform cannot prove about a launch, or that
// depends on host conditions it cannot see. None of them stops a deployment:
// a launch that really fails shows it in the engine log and instance state.
func launchWarnings(launch ResolvedLaunch, params Params, image string, compat ascendRuntimeCompat, engram engramLaunch) []messages.Warning {
	var warnings []messages.Warning
	if compat == ascendRuntimeCompatV1 {
		if params.NNodes > 1 {
			warnings = append(warnings, messages.NewWarning(messages.WarningAscendV1CompatMultiNode,
				map[string]any{"nnodes": params.NNodes, "runtime": params.RuntimeDriver, "image": image}))
		}
		if dp, set, symbolic, err := topologyInteger(launchArguments(launch), vllmDataParallelFlags...); err == nil && set && !symbolic && dp > 1 {
			warnings = append(warnings, messages.NewWarning(messages.WarningAscendV1CompatDataParallel,
				map[string]any{"dp": dp, "runtime": params.RuntimeDriver, "image": image}))
		}
	}
	if compat != ascendRuntimeCompatNone && inspectShellCommand(launch.Entrypoint, launch.Argv).shell {
		warnings = append(warnings, messages.NewWarning(messages.WarningAscendCompatManualShell,
			map[string]any{"image": image, "runtime": params.RuntimeDriver}))
	}
	if engram.unchecked {
		warnings = append(warnings, messages.NewWarning(messages.WarningEngramManualUnchecked,
			map[string]any{"flag": engramFlag}))
	}
	if strings.EqualFold(strings.TrimSpace(params.RuntimeDriver), proto.RuntimePreferenceUdocker) {
		code := ""
		switch {
		case engram.state.shared:
			code = messages.WarningEngramSharedMemoryRuntime
		case engram.state.uncertain:
			code = messages.WarningEngramConfigUncertain
		}
		if code != "" {
			warnings = append(warnings, messages.NewWarning(code, map[string]any{
				"runtime": params.RuntimeDriver, "flag": engramFlag, "shm_gib": engramSharedMemoryDefaultBytes >> 30,
			}))
		}
	}
	return warnings
}
