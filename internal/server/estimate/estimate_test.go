package estimate

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

const testGiB = int64(1) << 30

func TestComputeIncludesMMEncoderReserve(t *testing.T) {
	cfg := &modelconfig.Config{
		HiddenSize: 4096, TorchDtype: "bfloat16",
		VisionConfig: &modelconfig.TowerConfig{ImageSize: 448, PatchSize: 14},
	}
	const carried = int64(2) << 30
	b := Compute(Params{Config: cfg, WeightBytes: 20 * testGiB, Dtype: "bfloat16", TP: 2, ReserveBytes: carried})
	if b.MMEncoderBytes <= 0 || b.Grade != "rough_upper" {
		t.Fatalf("multimodal breakdown = %+v", b)
	}
	want := 10*testGiB + b.MMEncoderBytes/2 + b.ReserveBytes
	if b.PerGPUBytes != want || b.TotalBytes != want*2 {
		t.Fatalf("per GPU/total = %d/%d, want %d/%d", b.PerGPUBytes, b.TotalBytes, want, want*2)
	}
	if b.ReserveBytes != carried {
		t.Fatalf("reserve = %d, want the carried %d", b.ReserveBytes, carried)
	}
}

func TestComputeUtilizationAndSliceArePerGPUBudgets(t *testing.T) {
	const gib = int64(1 << 30)
	tests := []struct {
		name         string
		limit        int64
		util         float64
		wantCapacity int64
		wantFits     bool
	}{
		{name: "slice does not divide by tp", limit: 24 * gib, util: 0.9, wantCapacity: 24 * gib * 9 / 10, wantFits: true},
		{name: "fixed memory over slice", limit: 4 * gib, util: 0.9, wantCapacity: 4 * gib * 9 / 10, wantFits: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := Compute(Params{WeightBytes: 8 * gib, TP: 2, MaxModelLen: 0, GPUMemUtil: tt.util, VRAMLimitBytes: tt.limit})
			if b.CapacityBytes != tt.wantCapacity || b.Fits != tt.wantFits {
				t.Fatalf("breakdown = %+v, want capacity=%d fits=%v", b, tt.wantCapacity, tt.wantFits)
			}
			if b.GPUMemUtil != tt.util {
				t.Fatalf("util = %v, want %v", b.GPUMemUtil, tt.util)
			}
			if b.VRAMLimitBytes != tt.limit {
				t.Fatalf("vram limit = %d, want %d", b.VRAMLimitBytes, tt.limit)
			}
		})
	}
}

func TestComputeUsesGPUCapacityAndDoesNotClaimFitWhenCapacityUnknown(t *testing.T) {
	known := Compute(Params{WeightBytes: 8 * testGiB, TP: 1, GPUMemUtil: 0.8, GPUCapacityBytes: 16 * testGiB})
	if wantCapacity := 16 * testGiB * 8 / 10; known.CapacityBytes != wantCapacity || !known.Fits {
		t.Fatalf("known-capacity breakdown = %+v, want capacity %d", known, wantCapacity)
	}
	unknown := Compute(Params{WeightBytes: testGiB, TP: 1})
	if unknown.CapacityBytes != 0 || unknown.Fits {
		t.Fatalf("unknown capacity must not claim the model fits: %+v", unknown)
	}
}

// Qwen3-VL-Embedding-2B on a 24 GiB card at 0.9, and Qwen3-Reranker-8B on the
// same card: startup memory fits, one request of the model's own context does
// not, and the concurrency of that context is a real zero that has to be on
// the wire. A short context of the same model still fits, and a model with no
// measured KV cost does not invent a zero.
func TestComputeRefusesAFullContextTheKVPoolCannotHold(t *testing.T) {
	const gib = int64(1) << 30
	util := 0.9
	capacity := int64(float64(24*gib) * util)
	const perToken = 114688.0
	const maxLen = 262144
	const wantTokens = 146340
	pool := int64(wantTokens * perToken)
	embedding := Breakdown{
		WeightBytes:     capacity - pool,
		KVBytesPerToken: perToken,
		MaxModelLen:     maxLen,
		TensorParallel:  1,
		CapacityBytes:   capacity,
		Grade:           GradeMeasured,
	}
	Finalize(&embedding)
	if embedding.EstimatedKVTokens != wantTokens {
		t.Fatalf("tokens = %d, want %d", embedding.EstimatedKVTokens, wantTokens)
	}
	if embedding.Fits || embedding.MaxConcurrency == nil || *embedding.MaxConcurrency != 0 {
		t.Fatalf("embedding breakdown = %+v, want fits false and concurrency 0", embedding)
	}
	if !embedding.ContextExceedsKVPool() {
		t.Fatal("embedding context was not reported as exceeding the pool")
	}
	raw, err := json.Marshal(embedding)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"max_concurrency":0`) {
		t.Fatalf("wire hid a computed zero: %s", raw)
	}

	// Reranker: pool 4.36 GiB against 5.62 GiB for a 40960 context.
	gibf := float64(gib)
	reranker := Breakdown{
		WeightBytes:     int64(17.24 * gibf),
		KVBytesPerToken: (5.62 * gibf) / 40960,
		MaxModelLen:     40960,
		TensorParallel:  1,
		CapacityBytes:   int64(21.60 * gibf),
		Grade:           GradeMeasured,
	}
	Finalize(&reranker)
	if reranker.Fits || reranker.MaxConcurrency == nil || *reranker.MaxConcurrency != 0 || reranker.EstimatedKVTokens >= 40960 || reranker.EstimatedKVTokens <= 0 {
		t.Fatalf("reranker breakdown = %+v", reranker)
	}
	if reranker.KVPoolBytes >= int64(5.62*gibf) {
		t.Fatalf("reranker pool = %d, want it under one full context", reranker.KVPoolBytes)
	}
	suggested := 1
	for suggested<<1 > 0 && suggested<<1 <= reranker.EstimatedKVTokens {
		suggested <<= 1
	}
	if suggested != 16384 {
		t.Fatalf("reranker tokens = %d, largest power of two = %d, want 16384", reranker.EstimatedKVTokens, suggested)
	}

	short := embedding
	short.MaxModelLen = 1024
	short.MaxConcurrency = nil
	Finalize(&short)
	if !short.Fits || short.MaxConcurrency == nil || *short.MaxConcurrency < 1 || short.ContextExceedsKVPool() {
		t.Fatalf("short context = %+v", short)
	}

	unknown := Compute(Params{WeightBytes: gib, TP: 1, GPUMemUtil: 0.9, GPUCapacityBytes: 24 * gib, MaxModelLen: maxLen})
	if unknown.MaxConcurrency != nil || unknown.ContextExceedsKVPool() || !unknown.Fits {
		t.Fatalf("unmeasured KV must not refuse or invent a concurrency: %+v", unknown)
	}
	raw, err = json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "max_concurrency") {
		t.Fatalf("unmeasured breakdown invented a concurrency: %s", raw)
	}

	startupUtil := 0.8
	startup := Breakdown{
		WeightBytes: 20 * gib, KVBytesPerToken: perToken, MaxModelLen: 8192,
		TensorParallel: 1, CapacityBytes: int64(float64(16*gib) * startupUtil), Grade: GradeMeasured,
	}
	Finalize(&startup)
	if startup.Fits || startup.ContextExceedsKVPool() || startup.KVEstimateReason != KVReasonStartupExceedsCapacity {
		t.Fatalf("startup failure became a context refusal: %+v", startup)
	}
}

func TestComputeReportsKVUnavailableWhenStartupDoesNotFit(t *testing.T) {
	cfg := &modelconfig.Config{NumHiddenLayers: 32, NumAttentionHeads: 32, HeadDim: 128}
	b := Compute(Params{Config: cfg, WeightBytes: 20 * testGiB, TP: 1, MaxModelLen: 8192, GPUMemUtil: 0.8, GPUCapacityBytes: 16 * testGiB})
	if b.Fits || b.KVPoolBytes != 0 || b.EstimatedKVTokens != 0 {
		t.Fatalf("startup failure should not report usable KV capacity: %+v", b)
	}
	if b.KVEstimateReason == "" {
		t.Fatal("startup failure omitted KV estimate reason")
	}
}

func TestRequiredPerGPUExcludesDynamicKVPool(t *testing.T) {
	reserve := ReserveBytes(0, "Hopper", true, 0.9)
	got := RequiredPerGPUBytes(4*testGiB, 0, reserve, 1)
	want := 4*testGiB + reserve
	if got != want {
		t.Fatalf("startup fixed requirement = %d, want %d", got, want)
	}
	if reserve <= 0 {
		t.Fatalf("reserve = %d, want the engine's share charged", reserve)
	}
}

// A Compute caller that did not state a reserve still gets the conservative
// branch rather than none: a breakdown claiming a deployment fits on its weights
// alone is the one that misleads.
func TestComputeFallsBackToTheConservativeReserve(t *testing.T) {
	b := Compute(Params{WeightBytes: 8 * testGiB, TP: 1, GPUMemUtil: 0.9})
	if want := ReserveBytes(0, "", true, 0.9); b.ReserveBytes != want {
		t.Fatalf("reserve = %d, want the conservative %d", b.ReserveBytes, want)
	}
}

// The catalog records KV cost at a bf16 baseline, so the deployment's own dtype
// decides what the recorded figure is worth. Both directions have been wrong in
// this package's history: a float32 embedding model costs twice the recorded
// figure, and an fp8 KV cache costs half of it.
func TestComputeScalesKVCostByTheDeploymentsKVDtype(t *testing.T) {
	const recorded = 262144
	base := Params{WeightBytes: 8 * testGiB, KVBytesPerToken: recorded, MaxModelLen: 8192, TP: 1}

	tests := []struct {
		name         string
		kvDtype      string
		computeDtype string
		want         float64
	}{
		{name: "an unset KV dtype follows the bf16 default", want: recorded},
		{name: "auto follows the compute dtype", kvDtype: "auto", computeDtype: "bfloat16", want: recorded},
		{name: "fp8 KV halves the recorded cost", kvDtype: "fp8", computeDtype: "bfloat16", want: recorded / 2},
		{name: "an explicit fp8_e4m3 also halves it", kvDtype: "fp8_e4m3", want: recorded / 2},
		{name: "auto under a float32 model doubles it", kvDtype: "auto", computeDtype: "float32", want: recorded * 2},
		{name: "an fp8 KV cache still halves a float32 model", kvDtype: "fp8", computeDtype: "float32", want: recorded / 2},
		{name: "MXFP8 stores one byte", kvDtype: "mxfp8", computeDtype: "bfloat16", want: recorded / 2},
		{name: "NVFP4 stores half a byte", kvDtype: "nvfp4", computeDtype: "bfloat16", want: recorded / 4},
		{name: "SGLang's block-16 FP4 stores half a byte", kvDtype: "fp4_mx_block16", want: recorded / 4},
		{name: "TurboQuant K8V4 averages six bits", kvDtype: "turboquant_k8v4", want: recorded * 3 / 8},
		{name: "TurboQuant 3-bit stores three bits", kvDtype: "turboquant_3bit_nc", want: recorded * 3 / 16},
		{name: "fp16 KV stays at two bytes", kvDtype: "float16", computeDtype: "float32", want: recorded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			p.KVDtype, p.Dtype = tt.kvDtype, tt.computeDtype
			b := Compute(p)
			if b.KVBytesPerToken != tt.want {
				t.Fatalf("bytes/token = %v, want %v", b.KVBytesPerToken, tt.want)
			}
			if wantTotal := int64(tt.want) * 8192; b.KVTotalBytes != wantTotal {
				t.Fatalf("kv total = %d, want %d", b.KVTotalBytes, wantTotal)
			}
			// A narrower cache is exact arithmetic, not a weaker measurement.
			if b.Grade != GradeMeasured {
				t.Fatalf("grade = %q, want the measured grade to survive the scaling", b.Grade)
			}
		})
	}
}
