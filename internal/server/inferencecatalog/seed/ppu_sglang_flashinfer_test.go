package seed

import (
	"slices"
	"strings"
	"testing"
)

// selectsFlashInferOnSGLang reports whether SGLang 0.5.13 resolves a launch
// with these arguments to the FlashInfer attention backend regardless of the
// card. Two ways are decided by the arguments alone: naming the backend, and
// starting a diffusion language model with CUDA graphs on, where
// ServerArgs._handle_dllm_inference overrides whatever backend was chosen
// with "flashinfer". A launch that leaves the backend to SGLang's per-card
// default is not decided here; on a PPU card that default is fa3, as the
// engine log of the failed start shows ("Use fa3 backend by default", then
// "Attention backend is set to flashinfer because of enabling cuda graph in
// diffusion LLM inference").
func selectsFlashInferOnSGLang(args []string) bool {
	for _, flag := range []string{"--attention-backend", "--prefill-attention-backend", "--decode-attention-backend"} {
		if containsArgumentPair(args, flag, "flashinfer") {
			return true
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") && strings.HasSuffix(strings.SplitN(arg, "=", 2)[0], "attention-backend") &&
			strings.HasSuffix(arg, "=flashinfer") {
			return true
		}
	}
	diffusion := slices.ContainsFunc(args, func(arg string) bool {
		return arg == "--dllm-algorithm" || strings.HasPrefix(arg, "--dllm-algorithm=")
	})
	return diffusion && !slices.Contains(args, "--disable-cuda-graph")
}

// The PPU SGLang runner inherits the vendor's flashinfer_python
// 0.6.8.post1+v0.1.0.ppu2.1.0, and SGLang 0.5.13 refuses to start on the
// FlashInfer attention backend with anything below 0.6.12. The PPU spec of
// LLaDA2.1-mini started a diffusion language model with CUDA graphs on, which
// forces that backend, so every start exited within seconds on
// "flashinfer_python is installed with version 0.6.8.post1+v0.1.0.ppu2.1.0,
// which is less than the minimum required version 0.6.12" (GWKS-430). A spec
// on this image may not select the backend the image cannot serve.
func TestPPUSGLangSpecsDoNotSelectTheFlashInferBackend(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"removed LLaDA2.1-mini PPU launch", []string{"--enable-metrics", "--trust-remote-code", "--dllm-algorithm", "JointThreshold"}, true},
		{"diffusion without CUDA graphs", []string{"--dllm-algorithm", "JointThreshold", "--disable-cuda-graph"}, false},
		{"diffusion in one token", []string{"--dllm-algorithm=JointThreshold"}, true},
		{"named backend", []string{"--attention-backend", "flashinfer", "--disable-cuda-graph"}, true},
		{"named backend in one token", []string{"--decode-attention-backend=flashinfer"}, true},
		{"another backend", []string{"--attention-backend", "fa3"}, false},
		{"plain language model", []string{"--enable-metrics", "--trust-remote-code", "--disable-piecewise-cuda-graph"}, false},
	} {
		if got := selectsFlashInferOnSGLang(tc.args); got != tc.want {
			t.Errorf("%s: selectsFlashInferOnSGLang(%v) = %v, want %v", tc.name, tc.args, got, tc.want)
		}
	}

	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.Image != ppuRunnerSGLang {
				continue
			}
			checked++
			if selectsFlashInferOnSGLang(spec.Args) {
				t.Errorf("%s/%s starts SGLang on the FlashInfer backend, which %s cannot serve: %v",
					model.ModelID, spec.Name, ppuRunnerSGLang, spec.Args)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("no spec uses %s; the check covers nothing", ppuRunnerSGLang)
	}
}

// LLaDA2.1-mini keeps the NVIDIA spec, which runs on an image whose FlashInfer
// meets SGLang's floor, and has no PPU spec. Turning CUDA graphs off was tried
// on a PPU-ZW810E: the engine stays on fa3 and passes the version check, then
// the first batch fails in the fa3 backend (flashattention_backend.py
// init_forward_metadata, "'NoneType' object has no attribute 'shape'"). The
// diffusion path needs the FlashInfer backend, so a PPU spec comes back with
// an image whose flashinfer_python is at least 0.6.12.
func TestLLaDA21MiniOffersNoPPUSpec(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	vendors := map[string]int{}
	for _, model := range catalog.Models {
		if model.ModelID != "inclusionai/llada2.1-mini" {
			continue
		}
		for _, spec := range model.Specs {
			vendors[strings.ToLower(strings.TrimSpace(spec.GPUs.Vendor))]++
		}
	}
	if len(vendors) == 0 {
		t.Fatal("inclusionai/llada2.1-mini is not in the embedded catalog")
	}
	if vendors["ppu"] != 0 || vendors["nvidia"] == 0 {
		t.Fatalf("inclusionai/llada2.1-mini specs by vendor = %v, want an NVIDIA spec and none for PPU", vendors)
	}
}
