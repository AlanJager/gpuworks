package inferencecatalog

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
)

// perfTradeoffYAML is one vLLM spec with eager mode, a single-sequence cap and
// the given performance_tradeoffs block (indented as a spec field).
func perfTradeoffYAML(tradeoffs string) []byte {
	return []byte(`models:
  - model_id: org/model
    specs:
      - name: bf16-eager
        engine: vllm
        image: vllm/vllm-openai:latest
        weights:
          quantization: bf16
          sources:
            - huggingface: org/model
        gpus: {vendor: nvidia}
        args: [--enforce-eager, --max-num-seqs, "1", --gpu-memory-utilization, "0.7"]
` + tradeoffs)
}

func TestPerformanceTradeoffsParseAndNormalize(t *testing.T) {
	catalog, err := ParseYAML(perfTradeoffYAML(`        performance_tradeoffs:
          - param: " --enforce-eager "
            reason: hardware_plugin_requires
            detail_en: Graph capture hangs on this runner.
            detail_zh: 该镜像上图捕获会挂起。
          - param: --max-num-seqs
            reason: upstream_bug
            detail_en: Batched decode corrupts output.
            detail_zh: 批量解码输出错误。
            link: https://github.com/vllm-project/vllm/issues/1
          - param: --gpu-memory-utilization
            reason: memory_fit
            detail_en: The vision encoder needs the rest.
            detail_zh: 其余显存留给视觉编码器。
`))
	if err != nil {
		t.Fatal(err)
	}
	spec := catalog.Models[0].Specs[0]
	if len(spec.PerformanceTradeoffs) != 3 || spec.PerformanceTradeoffs[0].Param != "--enforce-eager" || spec.PerformanceTradeoffs[1].Link == "" {
		t.Fatalf("tradeoffs = %+v", spec.PerformanceTradeoffs)
	}
	if got := UnexplainedPerformanceFindings(spec); len(got) != 0 {
		t.Fatalf("stated tradeoffs left %+v unexplained", got)
	}
	// The catalog survives the YAML round trip with its reasons.
	data, err := MarshalYAML(catalog)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Models[0].Specs[0].PerformanceTradeoffs) != 3 {
		t.Fatalf("round trip lost tradeoffs:\n%s", data)
	}
}

func TestPerformanceTradeoffsRejectInvalidEntries(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		{name: "unknown reason", want: `unknown reason "faster"`, block: `
          - param: --enforce-eager
            reason: faster
            detail_en: x
            detail_zh: x
`},
		{name: "upstream bug without link", want: "requires link", block: `
          - param: --enforce-eager
            reason: upstream_bug
            detail_en: x
            detail_zh: x
`},
		{name: "link not a URL", want: "http(s) URL", block: `
          - param: --enforce-eager
            reason: upstream_bug
            detail_en: x
            detail_zh: x
            link: issue 18978
`},
		{name: "param the launch does not set", want: "does not set as a performance cost", block: `
          - param: --no-enable-prefix-caching
            reason: hardware_plugin_requires
            detail_en: x
            detail_zh: x
`},
		{name: "param set but not a cost", want: "does not set as a performance cost", block: `
          - param: --max-model-len
            reason: memory_fit
            detail_en: x
            detail_zh: x
`},
		{name: "missing chinese detail", want: "requires detail_en and detail_zh", block: `
          - param: --enforce-eager
            reason: hardware_plugin_requires
            detail_en: x
`},
		{name: "missing param", want: "requires param", block: `
          - reason: hardware_plugin_requires
            detail_en: x
            detail_zh: x
`},
		{name: "repeated param", want: "repeats param --enforce-eager", block: `
          - param: --enforce-eager
            reason: hardware_plugin_requires
            detail_en: x
            detail_zh: x
          - param: --enforce-eager
            reason: accuracy
            detail_en: x
            detail_zh: x
`},
		{name: "unknown field", want: "field evidence not found", block: `
          - param: --enforce-eager
            reason: hardware_plugin_requires
            detail_en: x
            detail_zh: x
            evidence: run.json
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseYAML(perfTradeoffYAML("        performance_tradeoffs:" + tc.block))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestUnexplainedPerformanceFindingsReadEveryLaunchInput(t *testing.T) {
	spec := InferenceSpec{
		Engine:        engines.SGLang,
		Args:          []string{"--max-running-requests", "2"},
		Entrypoint:    []string{"bash", "-c", "python -m sglang.launch_server --disable-cuda-graph"},
		ManualCommand: "python -m sglang.launch_server --disable-radix-cache --mem-fraction-static 0.5",
	}
	var got []string
	for _, finding := range UnexplainedPerformanceFindings(spec) {
		got = append(got, finding.Where+" "+finding.Param)
	}
	want := []string{"args --max-running-requests", "entrypoint --disable-cuda-graph", "manual_command --disable-radix-cache"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexplained = %q, want %q (info findings such as --mem-fraction-static are never unexplained)", got, want)
	}
}

func TestPoolingSpecsAreExemptOnlyFromDecodeCosts(t *testing.T) {
	spec := InferenceSpec{
		Engine: engines.VLLM,
		Args:   []string{"--enforce-eager", "--max-num-seqs", "1", "--no-enable-prefix-caching", "--no-enable-chunked-prefill"},
		API:    &APIContract{Operations: []APIOperation{{Type: "openai.embeddings"}, {Type: "openai.rerank"}}},
	}
	got := UnexplainedPerformanceFindings(spec)
	if len(got) != 1 || got[0].Cost != engines.CostChunkedPrefillDisabled {
		t.Fatalf("pooling spec unexplained = %+v, want only chunked prefill", got)
	}
	spec.API.Operations = append(spec.API.Operations, APIOperation{Type: "openai.chat"})
	if got := UnexplainedPerformanceFindings(spec); len(got) != 4 {
		t.Fatalf("a spec that also chats is not pooling-only: %+v", got)
	}
	spec.API = nil
	if PoolingOnly(spec) {
		t.Fatal("a spec without a declared contract is not pooling-only")
	}
}
