package deploy

import (
	"path/filepath"
	"strings"
	"sync"
)

// sglangServer is the SGLang application a launch runs. `sglang serve` fronts
// two of them: the LLM server and the diffusion server
// (sglang/multimodal_gen), which shares a handful of flags with the LLM
// server and refuses every other one ("error: unrecognized arguments").
type sglangServer int

const (
	// sglangServerUnknown is an executable outside the adapter's dialects.
	sglangServerUnknown sglangServer = iota
	sglangServerLLM
	sglangServerDiffusion
	// sglangServerDetected is `sglang serve` without --model-type: SGLang
	// picks the diffusion server by itself for a model directory that looks
	// like one (model_index.json or its diffusion registry,
	// sglang/cli/utils.py get_is_diffusion_model) and the LLM server
	// otherwise.
	sglangServerDetected
)

func sglangLaunchServer(launch LaunchShape) sglangServer {
	entrypoint := launch.Entrypoint
	if len(entrypoint) == 0 || isSGLangLLMEntrypoint(entrypoint) {
		// The adapter's own default is python -m sglang.launch_server.
		return sglangServerLLM
	}
	if !isSGLangServeEntrypoint(entrypoint) {
		return sglangServerUnknown
	}
	switch strings.ToLower(sglangServeModelType(entrypoint, launch.Args)) {
	case "llm":
		return sglangServerLLM
	case "diffusion":
		return sglangServerDiffusion
	case "", "auto":
		return sglangServerDetected
	}
	// An out-of-tree backend registered under its own name.
	return sglangServerUnknown
}

// sglangDiffusionFlagsSince lists the dictionary keys whose SGLang flag the
// diffusion server also accepts, with the first release that accepts it ("" =
// every release the catalog ships, v0.5.12 and later). Read from
// python/sglang/multimodal_gen/runtime/server_args/server_args.py at each
// release tag: --enable-cache-report arrived in v0.5.18 and --enable-metrics in
// v0.5.21. Any other key with an SGLang flag is an LLM server flag the
// diffusion server refuses - --mem-fraction-static, --context-length and the
// rest - so a diffusion launch states no default for it.
var sglangDiffusionFlagsSince = map[string]string{
	"log_level":           "",
	"quantization":        "",
	"trust_remote_code":   "",
	"data_parallel_size":  "",
	"enable_cache_report": "0.5.18",
	"enable_metrics":      "0.5.21",
}

// sglangDiffusionAccepts reports whether the diffusion server of this release
// takes the key's flag. A release nobody recorded is not assumed to have a
// flag that only newer releases accept.
func sglangDiffusionAccepts(key, version string) bool {
	if row, ok := sglangFlagRows()[key]; !ok || row.Platform {
		// Nothing the adapter emits for this key.
		return true
	}
	since, shared := sglangDiffusionFlagsSince[key]
	if !shared {
		return false
	}
	if since == "" {
		return true
	}
	return strings.TrimSpace(version) != "" && compareVersion(version, since) >= 0
}

// sglangFlagRows is SGLang's dictionary, built once: default applicability
// asks it for every key of every launch it judges.
var sglangFlagRows = sync.OnceValue(func() map[string]engineParam { return engineParams(EngineSGLang) })

// DefaultApplicable keeps SGLang's entrypoint dialect knowledge inside its
// adapter: a default goes out only where the server the launch runs takes
// the flag. The diffusion server takes only the flags it shares with the LLM
// server, some of them only from a given release (sglangDiffusionFlagsSince);
// `sglang serve` without --model-type may run either server, so a default
// goes out there only when both take it. Elsewhere the key is left to the
// engine and the deployer can still state it either way. Whether a catalog
// happens to contain a flag is deliberately not consulted: argument presence
// states a value, not a capability.
func (sglangEngine) DefaultApplicable(key string, launch LaunchShape) bool {
	switch sglangLaunchServer(launch) {
	case sglangServerLLM:
		return true
	case sglangServerDiffusion, sglangServerDetected:
		// The LLM server takes every flag in the dictionary.
		return sglangDiffusionAccepts(key, launch.EngineVersion)
	}
	// An explicit executable outside the adapter's known dialects cannot be
	// assumed to accept an SGLang-specific flag.
	return key != "enable_metrics"
}

// sglangServeModelType is the --model-type a `sglang serve` command states,
// or "" when it leaves the choice to SGLang's own detection.
func sglangServeModelType(entrypoint, args []string) string {
	if !isSGLangServeEntrypoint(entrypoint) {
		return ""
	}
	// An entrypoint JSON array may include application arguments after `serve`.
	// The engine parameter vector follows it in the final command.
	finalArgs := append(append([]string(nil), entrypoint[2:]...), args...)
	modelType := ""
	for index, token := range finalArgs {
		if token == "--model-type" && index+1 < len(finalArgs) {
			modelType = strings.TrimSpace(finalArgs[index+1])
			continue
		}
		if name, value, found := strings.Cut(token, "="); found && name == "--model-type" {
			modelType = strings.TrimSpace(value)
		}
	}
	// argparse-style repeated options use the last occurrence. This also gives
	// user residual args, appended after template args, the same precedence the
	// final SGLang command receives.
	return modelType
}

func isSGLangServeEntrypoint(entrypoint []string) bool {
	return len(entrypoint) >= 2 && filepath.Base(entrypoint[0]) == EngineSGLang && entrypoint[1] == "serve"
}

func isSGLangLLMEntrypoint(entrypoint []string) bool {
	if len(entrypoint) < 3 || entrypoint[1] != "-m" || entrypoint[2] != "sglang.launch_server" {
		return false
	}
	return strings.HasPrefix(filepath.Base(entrypoint[0]), "python")
}
