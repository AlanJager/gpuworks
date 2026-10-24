package deploy

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const maxResolvedPreStartScriptBytes = 64 << 10

// preflightResolvedLaunch checks constraints of the fully merged launch before
// it can be persisted or sent to an agent. preCompatScript is the preparation
// present before the platform added its own compatibility script. manualScript
// is checked through static words when possible; opaque vLLM and Engram entries
// fail closed while unrelated preparation remains executable.
func preflightResolvedLaunch(launch ResolvedLaunch, params Params, image string, compat ascendRuntimeCompat, preCompatScript, manualScript string) error {
	shell := inspectShellCommand(launch.Entrypoint, launch.Argv)
	if shell.commandString {
		manualScript = shell.script
	}
	manualEntry := shell.shell && !shell.scriptFile || manualScript != ""
	if compat == ascendRuntimeCompatV1 && params.NNodes > 1 {
		return messages.NewProductError(messages.ErrorAscendV1CompatMultiNode,
			fmt.Sprintf("the vLLM-Ascend rc2 A3 V1 compatibility rendezvous is local to one node and cannot coordinate a %d-node deployment; use the V2 model runner, Docker or nerdctl, or deploy on one node", params.NNodes),
			map[string]any{"nnodes": params.NNodes, "runtime": params.RuntimeDriver, "image": image})
	}
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
	if compat != ascendRuntimeCompatNone && (shell.shell || manualScript != "") {
		return messages.NewProductError(messages.ErrorAscendCompatManualUnsupported,
			"manual shell cannot prove which model runner and preparation the Ascend compatibility image will use; use engine mode or a JSON argv, or use Docker or nerdctl",
			map[string]any{"image": image, "runtime": params.RuntimeDriver})
	}
	if manualEntry {
		words, err := staticShellWords(manualScript)
		reject := !shell.commandString
		if shell.commandString {
			reject = err != nil || hasNestedShellCommand(words) || inspectEngramArguments(words).present
		}
		if reject {
			return messages.NewProductError(messages.ErrorEngramManualUnsupported,
				"manual shell cannot be checked for Engram and its required shared-memory settings; use engine mode or a JSON argv",
				map[string]any{"flag": "--engram-config"})
		}
	}
	command := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
	engram := inspectEngramArguments(command)
	runtime := strings.ToLower(strings.TrimSpace(params.RuntimeDriver))
	if (engram.shared || engram.uncertain) && (runtime == proto.RuntimePreferenceUdocker || runtime == "runveil") {
		return messages.NewProductError(messages.ErrorEngramSharedMemoryRuntime,
			fmt.Sprintf("Engram shared host memory, or a configuration whose shared-memory use cannot be proven, requires Docker or nerdctl; %s cannot run it safely", params.RuntimeDriver),
			map[string]any{"runtime": params.RuntimeDriver, "flag": "--engram-config"})
	}
	if len(launch.PreStartScript) > maxResolvedPreStartScriptBytes {
		return messages.NewProductError(messages.ErrorPreStartScriptTooLarge,
			fmt.Sprintf("pre_start_script is %d bytes, above the agent's %d-byte limit", len(launch.PreStartScript), maxResolvedPreStartScriptBytes),
			map[string]any{"bytes": len(launch.PreStartScript), "max_bytes": maxResolvedPreStartScriptBytes})
	}
	return nil
}
