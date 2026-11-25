package engines

import (
	"reflect"
	"testing"
)

func guarded(cost PerformanceCost, param, value string) PerformanceFinding {
	return PerformanceFinding{Cost: cost, Level: LevelGuarded, Param: param, Value: value}
}

func info(cost PerformanceCost, param, value string) PerformanceFinding {
	return PerformanceFinding{Cost: cost, Level: LevelInfo, Param: param, Value: value}
}

func TestScanPerformanceCostsGradesEachEngineSpelling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		engine string
		argv   []string
		env    map[string]string
		want   []PerformanceFinding
	}{
		{name: "vllm eager", engine: VLLM, argv: []string{"--enforce-eager"}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--enforce-eager", "")}},
		{name: "vllm eager explicitly false", engine: VLLM, argv: []string{"--enforce-eager=false"}},
		{name: "sglang eager", engine: SGLang, argv: []string{"--disable-cuda-graph"}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--disable-cuda-graph", "")}},
		// Each engine's spelling means nothing to the other engine.
		{name: "sglang flag on vllm", engine: VLLM, argv: []string{"--disable-cuda-graph", "--disable-radix-cache"}},
		{name: "vllm flag on sglang", engine: SGLang, argv: []string{"--enforce-eager", "--no-enable-prefix-caching"}},
		{name: "prefix caching", engine: VLLM, argv: []string{"--no-enable-prefix-caching"}, want: []PerformanceFinding{guarded(CostPrefixCacheDisabled, "--no-enable-prefix-caching", "")}},
		{name: "radix cache", engine: SGLang, argv: []string{"--disable-radix-cache"}, want: []PerformanceFinding{guarded(CostPrefixCacheDisabled, "--disable-radix-cache", "")}},
		{name: "concurrency guarded", engine: VLLM, argv: []string{"--max-num-seqs", "4"}, want: []PerformanceFinding{guarded(CostConcurrencyCapped, "--max-num-seqs", "4")}},
		{name: "concurrency inline", engine: VLLM, argv: []string{"--max-num-seqs=1"}, want: []PerformanceFinding{guarded(CostConcurrencyCapped, "--max-num-seqs", "1")}},
		{name: "concurrency info", engine: VLLM, argv: []string{"--max-num-seqs", "5"}, want: []PerformanceFinding{info(CostConcurrencyCapped, "--max-num-seqs", "5")}},
		{name: "concurrency info upper", engine: SGLang, argv: []string{"--max-running-requests", "8"}, want: []PerformanceFinding{info(CostConcurrencyCapped, "--max-running-requests", "8")}},
		{name: "concurrency fine", engine: VLLM, argv: []string{"--max-num-seqs", "9"}},
		{name: "concurrency sglang guarded", engine: SGLang, argv: []string{"--max-running-requests", "2"}, want: []PerformanceFinding{guarded(CostConcurrencyCapped, "--max-running-requests", "2")}},
		{name: "concurrency unparsable", engine: VLLM, argv: []string{"--max-num-seqs", "${SEQS}"}},
		{name: "chunked prefill vllm", engine: VLLM, argv: []string{"--no-enable-chunked-prefill"}, want: []PerformanceFinding{guarded(CostChunkedPrefillDisabled, "--no-enable-chunked-prefill", "")}},
		{name: "chunked prefill sglang", engine: SGLang, argv: []string{"--chunked-prefill-size", "-1"}, want: []PerformanceFinding{guarded(CostChunkedPrefillDisabled, "--chunked-prefill-size", "-1")}},
		{name: "small prefill sglang", engine: SGLang, argv: []string{"--chunked-prefill-size", "1024"}, want: []PerformanceFinding{info(CostPrefillBatchSmall, "--chunked-prefill-size", "1024")}},
		{name: "small prefill vllm", engine: VLLM, argv: []string{"--max-num-batched-tokens", "256"}, want: []PerformanceFinding{info(CostPrefillBatchSmall, "--max-num-batched-tokens", "256")}},
		{name: "prefill fine", engine: VLLM, argv: []string{"--max-num-batched-tokens", "2048"}},
		{name: "overlap", engine: SGLang, argv: []string{"--disable-overlap-schedule"}, want: []PerformanceFinding{guarded(CostOverlapScheduleDisabled, "--disable-overlap-schedule", "")}},
		{name: "gpu memory", engine: VLLM, argv: []string{"--gpu-memory-utilization", "0.7"}, want: []PerformanceFinding{info(CostKVMemoryReduced, "--gpu-memory-utilization", "0.7")}},
		{name: "gpu memory fine", engine: VLLM, argv: []string{"--gpu-memory-utilization", "0.8"}},
		{name: "static memory", engine: SGLang, argv: []string{"--mem-fraction-static", "0.6"}, want: []PerformanceFinding{info(CostKVMemoryReduced, "--mem-fraction-static", "0.6")}},
		{name: "sglang capture", engine: SGLang, argv: []string{"--cuda-graph-max-bs", "8"}, want: []PerformanceFinding{info(CostGraphCaptureNarrow, "--cuda-graph-max-bs", "8")}},
		{name: "sglang decode capture", engine: SGLang, argv: []string{"--cuda-graph-max-bs-decode", "8"}, want: []PerformanceFinding{info(CostGraphCaptureNarrow, "--cuda-graph-max-bs-decode", "8")}},
		{name: "sglang decode capture inline", engine: SGLang, argv: []string{"--cuda-graph-max-bs-decode=4"}, want: []PerformanceFinding{info(CostGraphCaptureNarrow, "--cuda-graph-max-bs-decode", "4")}},
		{name: "sglang decode capture at the limit", engine: SGLang, argv: []string{"--cuda-graph-max-bs-decode", "16"}},
		{name: "sglang prefill graph off", engine: SGLang, argv: []string{"--cuda-graph-backend-prefill", "disabled"}, want: []PerformanceFinding{info(CostGraphCaptureNarrow, "--cuda-graph-backend-prefill", "disabled")}},
		{name: "sglang prefill graph backend", engine: SGLang, argv: []string{"--cuda-graph-backend-prefill", "breakable"}},
		{name: "sglang prefill graph off, decode capped", engine: SGLang, argv: []string{"--cuda-graph-backend-prefill", "disabled", "--cuda-graph-max-bs-decode", "8"}, want: []PerformanceFinding{
			info(CostGraphCaptureNarrow, "--cuda-graph-backend-prefill", "disabled"), info(CostGraphCaptureNarrow, "--cuda-graph-max-bs-decode", "8"),
		}},
		{name: "sglang per-phase graph flags on vllm", engine: VLLM, argv: []string{"--cuda-graph-backend-prefill", "disabled", "--cuda-graph-max-bs-decode", "8"}},
		{name: "triton attention", engine: VLLM, argv: []string{"--attention-backend", "TRITON_ATTN"}, want: []PerformanceFinding{info(CostKernelFallback, "--attention-backend", "TRITON_ATTN")}},
		{name: "flash attention", engine: VLLM, argv: []string{"--attention-backend", "FLASH_ATTN"}},
		{name: "graph mode none", engine: VLLM, argv: []string{"--compilation-config", `{"cudagraph_mode":"NONE"}`}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--compilation-config.cudagraph_mode", "NONE")}},
		{name: "graph mode none dotted", engine: VLLM, argv: []string{"--compilation-config.cudagraph_mode=NONE"}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--compilation-config.cudagraph_mode", "NONE")}},
		{name: "graph mode none short json", engine: VLLM, argv: []string{"-O", `{"cudagraph_mode":"NONE"}`}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--compilation-config.cudagraph_mode", "NONE")}},
		{name: "mode zero without graph mode", engine: VLLM, argv: []string{"--compilation-config", `{"mode":0}`}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--compilation-config.mode", "0")}},
		// DeepSeek V4's recipe: no torch.compile, full decode graphs still replayed.
		{name: "mode zero with decode graphs", engine: VLLM, argv: []string{"--compilation-config", `{"mode":0,"cudagraph_mode":"FULL_DECODE_ONLY"}`}, want: []PerformanceFinding{info(CostGraphCaptureNarrow, "--compilation-config.cudagraph_mode", "FULL_DECODE_ONLY")}},
		// A dotted graph mode later in argv completes a JSON mode 0.
		{name: "mode zero merged with dotted graph mode", engine: VLLM, argv: []string{"--compilation-config", `{"mode":0}`, "--compilation-config.cudagraph_mode", "PIECEWISE"}},
		{name: "optimization level zero", engine: VLLM, argv: []string{"-O0"}, want: []PerformanceFinding{guarded(CostGraphDisabled, "-O", "0")}},
		{name: "optimization level zero spaced", engine: VLLM, argv: []string{"-O", "0", "--max-num-seqs", "64"}, want: []PerformanceFinding{guarded(CostGraphDisabled, "-O", "0")}},
		{name: "optimization level three", engine: VLLM, argv: []string{"-O3"}},
		// -O0 is compilation mode 0: an explicit graph mode keeps graphs.
		{name: "optimization level zero with decode graphs", engine: VLLM, argv: []string{"-O0", "--compilation-config", `{"cudagraph_mode":"FULL_DECODE_ONLY"}`}, want: []PerformanceFinding{info(CostGraphCaptureNarrow, "--compilation-config.cudagraph_mode", "FULL_DECODE_ONLY")}},
		{name: "level zero with graph mode", engine: VLLM, argv: []string{"--compilation-config", `{"level":0,"cudagraph_mode":"PIECEWISE"}`}},
		{name: "level zero without graph mode", engine: VLLM, argv: []string{"--compilation-config", `{"level":0}`}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--compilation-config.level", "0")}},
		{name: "use cudagraph false", engine: VLLM, argv: []string{"--compilation-config", `{"use_cudagraph":false}`}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--compilation-config.use_cudagraph", "false")}},
		{name: "use cudagraph false overrides mode zero", engine: VLLM, argv: []string{"--compilation-config", `{"mode":0,"use_cudagraph":false}`}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--compilation-config.use_cudagraph", "false")}},
		{name: "narrow max capture size", engine: VLLM, argv: []string{"--compilation-config.max_cudagraph_capture_size", "8"}, want: []PerformanceFinding{info(CostGraphCaptureNarrow, "--compilation-config.max_cudagraph_capture_size", "8")}},
		{name: "wide max capture size", engine: VLLM, argv: []string{"--compilation-config", `{"max_cudagraph_capture_size":16}`}},
		{name: "chunked prefill sglang inline", engine: SGLang, argv: []string{"--chunked-prefill-size=-1"}, want: []PerformanceFinding{guarded(CostChunkedPrefillDisabled, "--chunked-prefill-size", "-1")}},
		{name: "chunked prefill sglang then flag", engine: SGLang, argv: []string{"--chunked-prefill-size", "-1", "--disable-radix-cache"}, want: []PerformanceFinding{
			guarded(CostChunkedPrefillDisabled, "--chunked-prefill-size", "-1"),
			guarded(CostPrefixCacheDisabled, "--disable-radix-cache", ""),
		}},
		{name: "triton attention env", engine: VLLM, env: map[string]string{"VLLM_ATTENTION_BACKEND": "TRITON_ATTN"}, want: []PerformanceFinding{info(CostKernelFallback, "env:VLLM_ATTENTION_BACKEND", "TRITON_ATTN")}},
		{name: "flash attention env", engine: VLLM, env: map[string]string{"VLLM_ATTENTION_BACKEND": "FLASH_ATTN"}},
		// vLLM's parser takes underscores; the finding names the dashed flag.
		{name: "vllm underscore spelling", engine: VLLM, argv: []string{"--enforce_eager", "--max_num_seqs=2"}, want: []PerformanceFinding{
			guarded(CostGraphDisabled, "--enforce-eager", ""),
			guarded(CostConcurrencyCapped, "--max-num-seqs", "2"),
		}},
		{name: "sglang underscore spelling is not a flag", engine: SGLang, argv: []string{"--disable_cuda_graph"}},
		{name: "narrow capture sizes", engine: VLLM, argv: []string{"--compilation-config", `{"cudagraph_mode":"PIECEWISE","cudagraph_capture_sizes":[1,2,4]}`}, want: []PerformanceFinding{info(CostGraphCaptureNarrow, "--compilation-config.cudagraph_capture_sizes", "[1,2,4]")}},
		{name: "wide capture sizes", engine: VLLM, argv: []string{"--compilation-config", `{"cudagraph_capture_sizes":[1,2,4,8,16]}`}},
		{name: "speculative eager", engine: VLLM, argv: []string{"--speculative-config", `{"method":"mtp","enforce_eager":true}`}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--speculative-config.enforce_eager", "true")}},
		{name: "speculative eager dotted", engine: VLLM, argv: []string{"--speculative-config.enforce_eager", "true"}, want: []PerformanceFinding{guarded(CostGraphDisabled, "--speculative-config.enforce_eager", "true")}},
		{name: "speculative graphs", engine: VLLM, argv: []string{"--speculative-config", `{"method":"dflash","enforce_eager":false}`}},
		{name: "kernel env", engine: VLLM, env: map[string]string{"VLLM_USE_DEEP_GEMM": "0", "VLLM_HCU_USE_CUSTOM_QUANTIZATION_GEMM": "false", "VLLM_ROCM_USE_AITER": "1"}, want: []PerformanceFinding{
			info(CostKernelFallback, "env:VLLM_HCU_USE_CUSTOM_QUANTIZATION_GEMM", "false"),
			info(CostKernelFallback, "env:VLLM_USE_DEEP_GEMM", "0"),
		}},
		{name: "kernel env ignored on sglang", engine: SGLang, env: map[string]string{"VLLM_USE_DEEP_GEMM": "0"}},
		{name: "callable", engine: Callable, argv: []string{"--enforce-eager"}},
		{name: "values are not flags", engine: VLLM, argv: []string{"--served-model-name", "--enforce-eager-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanPerformanceCosts(tc.engine, tc.argv, tc.env)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ScanPerformanceCosts(%s, %q, %v)\n got %+v\nwant %+v", tc.engine, tc.argv, tc.env, got, tc.want)
			}
		})
	}
}

// The Kunlun Qwen3.8 recipe names torch.compile's "eager" backend while it
// still captures and replays piecewise graphs; it must not read as eager mode.
func TestEagerCompileBackendIsNotGraphDisabled(t *testing.T) {
	config := `{"mode":3,"cudagraph_mode":"PIECEWISE","backend":"eager","cudagraph_capture_sizes":[1,2,4,8,16,32]}`
	if got := ScanPerformanceCosts(VLLM, []string{"--compilation-config", config}, nil); len(got) != 0 {
		t.Fatalf("eager torch.compile backend graded as %+v", got)
	}
}

func TestScanKeepsArgvOrderAndConsumesValues(t *testing.T) {
	argv := []string{"--max-num-seqs", "2", "--enforce-eager", "--compilation-config", `{"cudagraph_mode":"FULL_DECODE_ONLY"}`, "--no-enable-prefix-caching"}
	want := []PerformanceFinding{
		guarded(CostConcurrencyCapped, "--max-num-seqs", "2"),
		guarded(CostGraphDisabled, "--enforce-eager", ""),
		guarded(CostPrefixCacheDisabled, "--no-enable-prefix-caching", ""),
		info(CostGraphCaptureNarrow, "--compilation-config.cudagraph_mode", "FULL_DECODE_ONLY"),
	}
	if got := ScanPerformanceCosts(VLLM, argv, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestPoolingExemptCoversOnlyDecodeCosts(t *testing.T) {
	exempt := map[PerformanceCost]bool{CostGraphDisabled: true, CostPrefixCacheDisabled: true, CostConcurrencyCapped: true}
	for _, cost := range PerformanceCosts() {
		if cost.PoolingExempt() != exempt[cost] {
			t.Errorf("%s PoolingExempt = %t, want %t", cost, cost.PoolingExempt(), exempt[cost])
		}
	}
}
