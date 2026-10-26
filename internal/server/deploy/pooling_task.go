package deploy

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// PoolingTaskArgs states an unambiguous API task for a new vLLM launch.
// v0.8.5/v0.9 use --task; v0.10.0 introduced --runner, retained by the
// v0.18+ images in the catalog. Unknown/older images get no guessed flags.
// Sources: github.com/vllm-project/vllm/blob/v0.9.0/vllm/config.py
// and github.com/vllm-project/vllm/blob/v0.10.0/vllm/config.py.
// This selects the execution task, not a model-specific classifier/template.
func PoolingTaskArgs(engine, version string, contract json.RawMessage, layers ...[]string) []string {
	if normalizeKey(engine) != EngineVLLM || strings.TrimSpace(version) == "" || compareVersion(version, "0.8.5") < 0 {
		return nil
	}
	var api inferenceapi.Contract
	if json.Unmarshal(contract, &api) != nil || len(api.Operations) == 0 {
		return nil
	}
	operation := ""
	for _, op := range api.Operations {
		if op.Type != inferenceapi.OperationEmbeddings && op.Type != inferenceapi.OperationRerank {
			return nil
		}
		if operation != "" && operation != op.Type {
			return nil
		}
		operation = op.Type
	}
	for _, layer := range layers {
		flags := flagSet(layer)
		if flags["--runner"] || flags["--task"] || isVLLMOmniLaunch(layer) {
			return nil
		}
	}
	if compareVersion(version, "0.10.0") >= 0 {
		return []string{"--runner", "pooling"}
	}
	if operation == inferenceapi.OperationRerank {
		return []string{"--task", "score"}
	}
	return []string{"--task", "embed"}
}
