package seed

import (
	"context"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/placement"
)

// The Apple silicon spec serves the same checkpoint as the vLLM specs through
// mlx-vlm, built from the upstream commit that loads it. It answers under the
// name the request asked for (the adapter), reads mlx-vlm's JSON /metrics, and
// admits a 48 GB Mac or larger: 21 GB resident, 30.6 GB at peak on a 10K-token
// prompt, measured on an M4 Max.
func TestInkling2MiniMLXSpec(t *testing.T) {
	_, spec := catalogSpec(t, inkling2MiniModel, "nvfp4-mlx-vlm-apple")
	if spec.Engine != "callable" || spec.Image != "gpuworks/gpuworks-runner:metal15.0-mlx-vlm0.8.1.dev20261121-r1" || spec.GPUs.Vendor != "apple" {
		t.Errorf("engine %q image %q vendor %q", spec.Engine, spec.Image, spec.GPUs.Vendor)
	}
	_, vllm := catalogSpec(t, inkling2MiniModel, inkling2MiniSpecs[0])
	if !slices.Equal(spec.Weights.Sources, vllm.Weights.Sources) || spec.WeightBytes != vllm.WeightBytes || spec.Weights.Quantization != "nvfp4" {
		t.Errorf("weights %+v (%d bytes) differ from the vLLM specs' checkpoint", spec.Weights, spec.WeightBytes)
	}
	want := []string{"--model", "${GPUWORKS_MODEL_PATH}", "--host", "${GPUWORKS_API_HOST}", "--port", "${GPUWORKS_API_PORT}"}
	if !slices.Equal(spec.Entrypoint, []string{"gpuworks-mlx-vlm"}) || !slices.Equal(spec.Args, want) {
		t.Errorf("entrypoint %v args %v", spec.Entrypoint, spec.Args)
	}
	// DSpark slowed long answers on the Mac; the drafter stays off.
	if slices.Contains(spec.Args, "--draft-model") {
		t.Errorf("args carry --draft-model: %v", spec.Args)
	}
	if spec.API == nil || spec.API.Metrics == nil || spec.API.Metrics.Format != "json" {
		t.Fatalf("api.metrics is not mlx-vlm's JSON document: %+v", spec.API)
	}
	for path, short := range map[string]string{
		"summary.in_flight":               "requests_running",
		"server.request_queue_depth":      "requests_waiting",
		"summary.prompt_tokens_total":     "prompt_tokens_total",
		"summary.completion_tokens_total": "generation_tokens_total",
		"summary.requests_completed":      "requests_success_total",
	} {
		if got := spec.API.Metrics.Map[path]; got.To != short {
			t.Errorf("metrics %s -> %q, want %s", path, got.To, short)
		}
	}
	resolve := func(memoryGiB float64) placement.Result {
		t.Helper()
		result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
			Count: 1, Vendor: "apple", Names: []string{"Apple M4 Max"}, MemoriesGiB: []float64{memoryGiB}, MinMemoryGiB: memoryGiB,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	// 48 GB Mac: 36 GiB for Metal by default (3/4 above 32 GiB).
	if result := resolve(36); result.Reason != "" || result.GPUMemoryMiBPerDevice != 32768 {
		t.Errorf("48 GB Mac: reason %q, claim %d MiB; want accepted with 32768", result.Reason, result.GPUMemoryMiBPerDevice)
	}
	// 36 GB Mac: 27 GiB, below the 30.6 GB peak.
	if result := resolve(27); result.Reason == "" || !strings.Contains(result.Reason, "32 GiB") {
		t.Errorf("36 GB Mac: reason %q, want a rejection naming 32 GiB", result.Reason)
	}
}
