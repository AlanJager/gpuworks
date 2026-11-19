package engines

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// vLLM spells its compilation config four ways: --compilation-config JSON,
// -O JSON, a dotted key (--compilation-config.cudagraph_mode NONE,
// -O.mode=0), and the optimization level -O0 .. -O3. The first three merge
// into one config, which is graded once under the canonical
// "--compilation-config.<key>" param; the level is graded as "-O".
var (
	compilationSpellings = []string{compilationConfigFlag, "--compilation_config", "-O"}
	speculativeSpellings = []string{speculativeConfigFlag, "--speculative_config"}
)

// vllmFallbackEnv are vLLM environment switches whose false value turns a
// faster kernel path off.
var vllmFallbackEnv = map[string]struct{}{
	"VLLM_USE_DEEP_GEMM":                    {},
	"VLLM_ROCM_USE_AITER":                   {},
	"VLLM_ROCM_USE_AITER_MOE":               {},
	"VLLM_USE_FLASHINFER_MOE_FP8":           {},
	"VLLM_USE_TRTLLM_ATTENTION":             {},
	"VLLM_HCU_USE_CUSTOM_QUANTIZATION_GEMM": {},
}

func scanVLLM(argv []string, env map[string]string) []PerformanceFinding {
	var findings []PerformanceFinding
	compilation, speculative := map[string]any{}, map[string]any{}
	level := ""
	for i := 0; i < len(argv); i++ {
		token := strings.TrimSpace(argv[i])
		name, inline, hasInline := strings.Cut(token, "=")
		if value, ok := optimizationLevel(name, inline, hasInline, argv, &i); ok {
			level = value
			continue
		}
		if mergeConfig(compilationSpellings, compilation, name, inline, hasInline, argv, &i) ||
			mergeConfig(speculativeSpellings, speculative, name, inline, hasInline, argv, &i) {
			continue
		}
		findings = append(findings, scanFlag(vllmFlagRules, true, argv, &i)...)
	}
	findings = append(findings, compilationFindings(compilation, level)...)
	findings = append(findings, speculativeFindings(speculative)...)
	return append(findings, vllmEnvFindings(env)...)
}

// optimizationLevel recognizes -O0 .. -O3 and "-O <digit>"; a JSON or dotted
// -O is a compilation config, not a level.
func optimizationLevel(name, inline string, hasInline bool, argv []string, i *int) (string, bool) {
	if len(name) == 3 && strings.HasPrefix(name, "-O") && name[2] >= '0' && name[2] <= '9' && !hasInline {
		return name[2:], true
	}
	if name != "-O" {
		return "", false
	}
	value := inline
	if !hasInline && *i+1 < len(argv) {
		value = strings.TrimSpace(argv[*i+1])
	}
	if len(value) == 1 && value[0] >= '0' && value[0] <= '9' {
		if !hasInline {
			*i++
		}
		return value, true
	}
	return "", false
}

// mergeConfig folds one JSON or dotted spelling of a config flag into config.
// It reports whether the token was such a spelling.
func mergeConfig(spellings []string, config map[string]any, name, inline string, hasInline bool, argv []string, i *int) bool {
	for _, flag := range spellings {
		if name == flag {
			value := inline
			if !hasInline {
				value = takeValue(argv, i)
			}
			var parsed map[string]any
			if json.Unmarshal([]byte(value), &parsed) == nil {
				for key, item := range parsed {
					config[key] = item
				}
			}
			return true
		}
		if key, ok := strings.CutPrefix(name, flag+"."); ok && key != "" {
			value := inline
			if !hasInline {
				value = takeValue(argv, i)
			}
			config[key] = dottedValue(value)
			return true
		}
	}
	return false
}

// dottedValue reads a dotted config value the way vLLM does: JSON when it
// parses (numbers, booleans, lists), the raw string otherwise.
func dottedValue(raw string) any {
	var parsed any
	if json.Unmarshal([]byte(raw), &parsed) == nil {
		return parsed
	}
	if b, err := strconv.ParseBool(raw); err == nil {
		return b
	}
	return raw
}

// compilationFindings grades the merged compilation config and the last
// optimization level written (empty when none).
func compilationFindings(config map[string]any, level string) []PerformanceFinding {
	param := func(key string) string { return compilationConfigFlag + "." + key }
	var findings []PerformanceFinding
	mode, hasMode := configString(config, "cudagraph_mode")
	switch strings.ToUpper(mode) {
	case "NONE":
		findings = append(findings, PerformanceFinding{Cost: CostGraphDisabled, Level: LevelGuarded, Param: param("cudagraph_mode"), Value: mode})
	case "FULL_DECODE_ONLY":
		findings = append(findings, PerformanceFinding{Cost: CostGraphCaptureNarrow, Level: LevelInfo, Param: param("cudagraph_mode"), Value: mode})
	}
	if use, ok := config["use_cudagraph"].(bool); ok && !use {
		findings = append(findings, PerformanceFinding{Cost: CostGraphDisabled, Level: LevelGuarded, Param: param("use_cudagraph"), Value: "false"})
		hasMode = true
	}
	// Without a graph mode, vLLM derives NONE from a compilation mode other
	// than VLLM_COMPILE (3); -O0 is that mode 0 and likewise still replays
	// graphs when a graph mode is set. A "backend" of "eager" is only the
	// torch.compile backend and does not stop graph replay.
	if !hasMode {
		if level == "0" {
			findings = append(findings, PerformanceFinding{Cost: CostGraphDisabled, Level: LevelGuarded, Param: "-O", Value: level})
		}
		for _, key := range []string{"mode", "level"} {
			if n, ok := config[key].(float64); ok && n == 0 {
				findings = append(findings, PerformanceFinding{Cost: CostGraphDisabled, Level: LevelGuarded, Param: param(key), Value: "0"})
			}
		}
	}
	if sizes, ok := config["cudagraph_capture_sizes"].([]any); ok && len(sizes) > 0 {
		largest := 0.0
		for _, size := range sizes {
			if n, ok := size.(float64); ok && n > largest {
				largest = n
			}
		}
		if largest < narrowCaptureSize {
			encoded, _ := json.Marshal(sizes)
			findings = append(findings, PerformanceFinding{Cost: CostGraphCaptureNarrow, Level: LevelInfo, Param: param("cudagraph_capture_sizes"), Value: string(encoded)})
		}
	}
	if n, ok := config["max_cudagraph_capture_size"].(float64); ok && n > 0 && n < narrowCaptureSize {
		findings = append(findings, PerformanceFinding{Cost: CostGraphCaptureNarrow, Level: LevelInfo, Param: param("max_cudagraph_capture_size"), Value: fmt.Sprint(n)})
	}
	return findings
}

func speculativeFindings(config map[string]any) []PerformanceFinding {
	switch eager := config["enforce_eager"].(type) {
	case bool:
		if eager {
			return []PerformanceFinding{{Cost: CostGraphDisabled, Level: LevelGuarded, Param: speculativeConfigFlag + ".enforce_eager", Value: "true"}}
		}
	case string:
		if isTrue(eager) {
			return []PerformanceFinding{{Cost: CostGraphDisabled, Level: LevelGuarded, Param: speculativeConfigFlag + ".enforce_eager", Value: eager}}
		}
	}
	return nil
}

func vllmEnvFindings(env map[string]string) []PerformanceFinding {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var findings []PerformanceFinding
	for _, key := range keys {
		value := strings.TrimSpace(env[key])
		_, fallback := vllmFallbackEnv[key]
		switch {
		case fallback && isFalse(value):
		case key == "VLLM_ATTENTION_BACKEND" && strings.EqualFold(value, "TRITON_ATTN"):
		default:
			continue
		}
		findings = append(findings, PerformanceFinding{Cost: CostKernelFallback, Level: LevelInfo, Param: "env:" + key, Value: value})
	}
	return findings
}

func configString(config map[string]any, key string) (string, bool) {
	value, ok := config[key]
	if !ok {
		return "", false
	}
	if s, ok := value.(string); ok {
		return s, true
	}
	return fmt.Sprint(value), true
}
