package deploy

import (
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// PrioritySchedulingCommandRefusal checks the executable that will receive
// adapter-generated flags, in addition to the engine's policy and target.
// Unknown entrypoints and image-owned commands do not establish LLM support.
func PrioritySchedulingCommandRefusal(engine, vendor string, pd bool, templateArgs, userArgs []string, overrides ContainerOverrides, templateEntrypoint []string) *PriorityRefusal {
	if refusal := PrioritySchedulingRefusal(engine, vendor, pd, nil, nil); refusal != nil {
		return refusal
	}
	command, err := ResolveEffectiveCommand(overrides, templateEntrypoint)
	unsupported := func() *PriorityRefusal {
		return &PriorityRefusal{Code: messages.ErrorPrioritySchedulingLaunchUnsupported, Params: map[string]any{"engine": engine}}
	}
	if err != nil || command.Mode == CommandManual || strings.TrimSpace(overrides.CmdDisplay) != "" || (command.Mode == CommandImageEntry && len(command.Entrypoint) == 0) {
		return unsupported()
	}
	_, template := residualLayers(templateArgs, userArgs)
	args := append(append([]string(nil), template...), userArgs...)
	entry := command.Entrypoint
	llm := false
	if engine == EngineSGLang {
		llm = sglangLaunchServer(LaunchShape{Entrypoint: entry, Args: args}) == sglangServerLLM
	} else {
		// --omni selects a pipeline even when the executable is vllm serve.
		allArgs := append(append([]string(nil), entry...), args...)
		serve := len(entry) >= 2 && filepath.Base(entry[0]) == EngineVLLM && entry[1] == "serve"
		apiServer := len(entry) >= 3 && strings.HasPrefix(filepath.Base(entry[0]), "python") && entry[1] == "-m" && entry[2] == "vllm.entrypoints.openai.api_server"
		llm = !isVLLMOmniLaunch(allArgs) && (len(entry) == 0 || serve || apiServer)
	}
	if !llm {
		return unsupported()
	}
	// Entrypoint arrays can themselves carry policy flags before the adapter
	// arguments. Check the complete effective layers, with user values last.
	return PrioritySchedulingRefusal(engine, vendor, pd, append(append([]string(nil), entry...), templateArgs...), userArgs)
}
