package proxy

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Read the concrete launch, since a template or manual command can override
// both the model path and its alias independently of the public routing name.
func instanceModelName(dep *store.Deployment, instance *store.Instance) string {
	if dep.Engine != engines.SGLang && dep.Engine != engines.VLLM {
		return ""
	}
	var desired proto.InstanceDesiredState
	if json.Unmarshal([]byte(instance.DesiredStateJSON), &desired) != nil {
		return ""
	}
	args, scriptArgs, ok := deploy.LaunchArgs(desired.ResolvedLaunch)
	if !ok {
		return ""
	}
	for _, flag := range []string{"--served-model-name", "--model-id", "--model-path", "--model"} {
		if value, found := deploy.LaunchFlag(args, flag); found {
			return value
		}
		if hasUnfixedFlag(args, flag) {
			return ""
		}
	}
	// An engine that carries the model as a positional parameter is read
	// through the engine's own split of its command, so the separator token is
	// not restated here.
	if name, ok := positionalModelName(dep.Engine, proto.ResolvedLaunch{Entrypoint: desired.Entrypoint, Argv: desired.Argv}); ok {
		return name
	}
	// A manual command keeps its tokens inside a shell script, where the engine
	// command starts after whatever the script did first (set -a, exports). Each
	// suffix of those tokens is offered to the same rule.
	for start := range scriptArgs {
		if name, ok := positionalModelName(dep.Engine, proto.ResolvedLaunch{Entrypoint: scriptArgs[start:]}); ok {
			return name
		}
	}
	return desired.ModelMount
}

// positionalModelName reports a model path an engine carries as a positional
// parameter (vLLM's `serve <model>`) rather than as a flag. The engine's own
// SplitCommand answers whether the command is its own shape and where its
// parameters begin, so no executable spelling is restated here; a launch the
// engine does not recognize has no positional model of its own.
func positionalModelName(engine string, launch proto.ResolvedLaunch) (string, bool) {
	_, params := deploy.SplitCommandForEngine(engine, launch)
	if len(params) == 0 || strings.HasPrefix(params[0], "-") {
		return "", false
	}
	return params[0], true
}

// hasUnfixedFlag reports whether flag is set to a value the shell would still
// expand, which names no model the proxy can rely on.
func hasUnfixedFlag(args []string, flag string) bool {
	for index, arg := range args {
		if (arg == flag && index+1 < len(args)) || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
