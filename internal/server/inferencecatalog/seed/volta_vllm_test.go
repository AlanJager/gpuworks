package seed

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// voltaRunner is the only engine image built for sm_70. It stays out of the
// system image catalog (catalog: false in the runner matrix), so a V100
// reaches it only through a spec that names it and pins cc 7.0.
const voltaRunner = "gpuworks/gpuworks-runner:cuda12.8-v100-vllm1cat1.5.1-r1"

// Volta specs were run on Aliyun gn6v (V100-SXM2-16GB) and gn6e
// (V100-SXM2-32GB) with this runner: V100 has no bf16, and 1Cat-vLLM was only
// verified with its Volta attention backend named explicitly.
func TestVoltaSpecsUseTheVoltaRunnerWithVerifiedArgs(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	volta := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			where := model.ModelID + "/" + spec.Name
			pinned := spec.GPUs.Vendor == "nvidia" && spec.GPUs.CC == "7.0"
			if (spec.Image == voltaRunner) != pinned {
				t.Errorf("%s: image %s with gpus %+v; the Volta runner and cc 7.0 go together", where, spec.Image, spec.GPUs)
				continue
			}
			if !pinned {
				continue
			}
			volta++
			args := strings.Join(spec.Args, " ")
			if spec.Engine != "vllm" || !strings.Contains(args, "--dtype float16") || !strings.Contains(args, "--attention-backend FLASH_ATTN_V100") {
				t.Errorf("%s: engine %s args %q", where, spec.Engine, args)
			}
			if !strings.HasPrefix(spec.DisplayNameEN, "V100 (Volta)") || !strings.HasPrefix(spec.DisplayNameZHCN, "V100 (Volta)") {
				t.Errorf("%s is named %q / %q", where, spec.DisplayNameZHCN, spec.DisplayNameEN)
			}
		}
	}
	if volta == 0 {
		t.Fatal("no spec serves V100")
	}
}

// A V100 node gets the Volta spec of Qwen3-0.6B, and no other NVIDIA card does.
func TestQwen3SmallPicksTheVoltaSpecOnlyOnV100(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(catalog.Models, func(m inferencecatalog.InferenceModel) bool { return m.ModelID == "qwen/qwen3-0.6b" })
	if index < 0 {
		t.Fatal("qwen/qwen3-0.6b is missing")
	}
	model := catalog.Models[index]
	specs := make([]chipmatch.Spec, len(model.Specs))
	for i, spec := range model.Specs {
		specs[i] = chipmatch.Spec{Name: spec.Name, GPUs: chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}, Quantization: spec.Weights.Quantization}
	}
	for _, card := range []struct {
		name, cc string
		volta    bool
	}{
		{"Tesla V100-SXM2-16GB", "7.0", true},
		{"Tesla V100-SXM2-32GB", "7.0", true},
		{"Tesla T4", "7.5", false},
		{"NVIDIA A10", "8.6", false},
	} {
		selection := chipmatch.Select(chipmatch.ReportedGPU(card.name, "nvidia", "", card.cc), specs, nil)
		got := selection.SpecIndex >= 0 && model.Specs[selection.SpecIndex].Image == voltaRunner
		if got != card.volta {
			t.Errorf("%s (cc %s): selected %+v, Volta spec = %v", card.name, card.cc, selection, got)
		}
	}
}
