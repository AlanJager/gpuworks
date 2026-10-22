package templateprobe

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/engines"
)

type L2CommandInput struct {
	Engine   string      `json:"engine"`
	Model    string      `json:"model"`
	BaseArgv []string    `json:"base_argv,omitempty"`
	Config   ModelConfig `json:"config"`
}

type L2Command struct {
	Engine     string             `json:"engine"`
	Argv       []string           `json:"argv"`
	Truncation TruncationDecision `json:"truncation"`
}

// BuildL2Command returns a tokenized dummy-load command. Probe-owned flags are
// stripped from BaseArgv and re-added exactly once with TP=1 and PP=1.
func BuildL2Command(input L2CommandInput) (L2Command, error) {
	engine := strings.ToLower(strings.TrimSpace(input.Engine))
	model := strings.TrimSpace(input.Model)
	if model == "" {
		return L2Command{}, fmt.Errorf("model is required")
	}
	decision := DecideTruncation(input.Config)
	if !decision.Allowed {
		return L2Command{Engine: engine, Truncation: decision}, nil
	}
	override, err := json.Marshal(map[string]int{"num_hidden_layers": decision.MinLayers})
	if err != nil {
		return L2Command{}, err
	}
	var defaults, owned []string
	switch engine {
	case engines.VLLM:
		defaults = []string{engines.VLLM, "serve", model}
		owned = []string{"--load-format", "--hf-overrides", "--tensor-parallel-size", "--pipeline-parallel-size", "--tp", "--pp"}
	case engines.SGLang:
		defaults = []string{"python", "-m", "sglang.launch_server", "--model-path", model}
		owned = []string{"--load-format", "--json-model-override-args", "--tp", "--tp-size", "--tensor-parallel-size", "--pp-size", "--pipeline-parallel-size"}
	default:
		return L2Command{}, fmt.Errorf("unsupported engine %q", input.Engine)
	}
	argv := append([]string(nil), input.BaseArgv...)
	if len(argv) == 0 {
		argv = defaults
	}
	argv, err = stripOwnedFlags(argv, owned)
	if err != nil {
		return L2Command{}, err
	}
	if engine == engines.VLLM {
		argv = append(argv,
			"--load-format", "dummy",
			"--tensor-parallel-size", "1",
			"--pipeline-parallel-size", "1",
			"--hf-overrides", string(override),
		)
	} else {
		argv = append(argv,
			"--load-format", "dummy",
			"--tp-size", "1",
			"--pp-size", "1",
			"--json-model-override-args", string(override),
		)
	}
	return L2Command{Engine: engine, Argv: argv, Truncation: decision}, nil
}

func stripOwnedFlags(argv, owned []string) ([]string, error) {
	set := make(map[string]struct{}, len(owned))
	for _, flag := range owned {
		set[flag] = struct{}{}
	}
	out := make([]string, 0, len(argv))
	for index := 0; index < len(argv); index++ {
		token := argv[index]
		name := token
		inline := false
		if split := strings.IndexByte(token, '='); split >= 0 {
			name, inline = token[:split], true
		}
		if _, found := set[name]; !found {
			out = append(out, token)
			continue
		}
		if !inline {
			if index+1 >= len(argv) || strings.HasPrefix(argv[index+1], "-") {
				return nil, fmt.Errorf("probe-owned flag %s has no value", name)
			}
			index++
		}
	}
	return out, nil
}
