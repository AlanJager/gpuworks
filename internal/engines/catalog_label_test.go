package engines

import "testing"

func TestCatalogEngineForLabel(t *testing.T) {
	for label, want := range map[string]string{
		"vllm-ascend": VLLM, "vllm-dcu": VLLM, "vllm-hggc": VLLM,
		"sglang-npu": SGLang, "sglang-dcu": SGLang, "sglang-hggc": SGLang,
		" vLLM ": VLLM, "sglang": SGLang, "vllm-omni": VLLMOmni, "callable": Callable,
		"vllm-future": "", "systemone": "", "": "",
	} {
		if got := CatalogEngineForLabel(label); got != want {
			t.Errorf("CatalogEngineForLabel(%q) = %q, want %q", label, got, want)
		}
	}
}
