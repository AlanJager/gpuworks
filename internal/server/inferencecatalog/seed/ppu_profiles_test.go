package seed

import "testing"

// The PPU runner images a profile may name. Each profile names the runner of
// the engine its passing run served with: the runs used the vendor's ASLLM
// images, whose vLLM/SGLang builds the hggc runners are built on, or the
// earlier hggc runners. The ASLLM archives themselves sit on an intranet host
// users cannot reach, so no profile names them.
const (
	ppuRunnerVLLM   = "gpuworks/gpuworks-runner:hggc13.0-vllm0.23.0-r2"
	ppuRunnerSGLang = "gpuworks/gpuworks-runner:hggc13.0-sglang0.5.13-r2"
)

type ppuProfileExpectation struct {
	engine      string
	image       string
	revision    string
	weightBytes int64
}

func TestEmbeddedCatalogIncludesDirectPPUProfiles(t *testing.T) {
	want := map[string]map[string]ppuProfileExpectation{
		"baidu/ernie-4.5-21b-a3b-pt": {
			"bf16-vllm-ppu-87db9548": {"vllm", ppuRunnerVLLM, "87db95487941cb39592ee0abca3b9155a6d19c5c", 43_906_845_592},
		},
		"google/translategemma-4b-it": {
			"bf16-vllm-ppu-10042cb0": {"vllm", ppuRunnerVLLM, "10042cb0e6e7fdce748996a71dc3dc432a4e0c89", 8_600_277_880},
		},
		"internlm/intern-s1-mini": {
			"bf16-vllm-ppu-0f25c8f6": {"vllm", ppuRunnerVLLM, "0f25c8f60f4190a18dd6620c94bdccedbebc208d", 17_077_718_216},
		},
		"liquidai/lfm2.5-1.2b-base": {
			"bf16-vllm-ppu-7453bca9": {"vllm", ppuRunnerVLLM, "7453bca97ca1e67754c4035a4b4c584e1c9dd725", 2_340_697_936},
		},
		"liquidai/lfm2.5-1.2b-jp": {
			"bf16-vllm-ppu-58ef35ca": {"vllm", ppuRunnerVLLM, "58ef35ca21bb2d3ef6e320a938765a9727aefc5d", 2_340_697_936},
		},
		"liquidai/lfm2.5-1.2b-jp-202606": {
			"bf16-vllm-ppu-52b8b447": {"vllm", ppuRunnerVLLM, "52b8b4475311a63bf839c6494f78f8ad59d13515", 2_340_697_936},
		},
		"liquidai/lfm2.5-8b-a1b": {
			"bf16-vllm-ppu-5dd22602": {"vllm", ppuRunnerVLLM, "5dd22602c2e9f6a097b1de4c4efe0658b605015c", 16_936_006_912},
		},
		"mistralai/ministral-3-3b-reasoning-2512": {
			"bf16-vllm-ppu-4a36357c": {"vllm", ppuRunnerVLLM, "4a36357c811bf511a7b625d132e12f22408aac91", 15_396_476_008},
		},
		"mistralai/ministral-3-8b-reasoning-2512": {
			"bf16-vllm-ppu-81eaece1": {"vllm", ppuRunnerVLLM, "81eaece1948f3875421d9a45bc55487d10e2d894", 35_672_239_168},
		},
		"mistralai/ministral-3-14b-reasoning-2512": {
			"bf16-vllm-ppu-51f9210f": {"vllm", ppuRunnerVLLM, "51f9210f3cd20f3452a80d5819d15dc61cc50630", 55_780_274_136},
		},
		"microsoft/phi-4-multimodal-instruct": {
			"bf16-vllm-ppu-93f923e1": {"vllm", ppuRunnerVLLM, "93f923e1a7727d1c4f446756212d9d3e8fcc5d81", 11_149_227_208},
		},
		"opengvlab/internvl3_5-8b": {
			"bf16-vllm-ppu-9bb6a56a": {"vllm", ppuRunnerVLLM, "9bb6a56ad9cc69db95e2d4eeb15a52bbcac4ef79", 17_056_729_432},
		},
		"google/gemma-4-e4b-it-qat-w4a16-ct": {
			"int4-vllm-ppu-6cd26aaa": {"vllm", ppuRunnerVLLM, "6cd26aaa2357fb2bad8c51699a7558a4d1a965bb", 11_513_861_828},
		},
		"hugging-quants/meta-llama-3.1-8b-instruct-awq-int4": {
			"awq-vllm-ppu-db1f81ad": {"vllm", ppuRunnerVLLM, "db1f81ad4b8c7e39777509fac66c652eb0a52f91", 5_727_938_576},
		},
		"nvidia/nvidia-nemotron-3.5-lightning-30b-a3b-bf16": {
			"bf16-vllm-ppu-a9904d24": {"vllm", ppuRunnerVLLM, "a9904d24bcc1d289a1950fa9d2b978c47cf903b9", 65_827_374_264},
		},
		"qwen/qwen2.5-coder-1.5b-instruct": {
			"bf16-vllm-ppu-2e1fd397": {"vllm", ppuRunnerVLLM, "2e1fd397ee46e1388853d2af2c993145b0f1098a", 3_087_467_144},
		},
		"qwen/qwen3-32b-awq": {
			"awq-vllm-ppu-0499c3ac": {"vllm", ppuRunnerVLLM, "0499c3ac83fdef8810b907a23894ba91e95eddd8", 19_325_481_744},
		},
		"qwen/qwen3.5-27b-gptq-int4": {
			"int4-vllm-ppu-8f0c09f2": {"vllm", ppuRunnerVLLM, "8f0c09f227ae570e79617c6d9172b59df9c16081", 30_235_264_416},
		},
		"qwen/qwen3.5-35b-a3b-gptq-int4": {
			"int4-vllm-ppu-3af5ca29": {"vllm", ppuRunnerVLLM, "3af5ca2972faf6de1fd6f4efc4d8d319ca751e8b", 24_419_939_760},
		},
		"qwen/qwen3-30b-a3b": {
			"bf16-vllm-ppu-ad44e777": {"vllm", ppuRunnerVLLM, "ad44e777bcd18fa416d9da3bd8f70d33ebb85d39", 61_066_575_648},
		},
		"qwen/qwen3guard-gen-8b": {
			"bf16-vllm-ppu-4505cb1a": {"vllm", ppuRunnerVLLM, "4505cb1a6f1864f21f8b27f7daf1b9a1aab6edbb", 16_381_516_776},
		},
		"zai-org/glm-4.7-flash": {
			"bf16-vllm-ppu-7dd20894": {"vllm", ppuRunnerVLLM, "7dd20894a642a0aa287e9827cb1a1f7f91386b67", 62_444_175_504},
		},

		// Profiles taken from the same run records for the fixed-topology and
		// multimodal cases. Their image is the one their own passing run used,
		// which differs per engine and per ASLLM release.
		"qwen/qwen3-14b": {
			"fp8-sglang-ppu-no-piecewise-9a283b4a": {"sglang", ppuRunnerSGLang, "9a283b4a5efbc09ce247e0ae5b02b744739e525a", 16_326_253_296},
		},
		"qwen/qwen3-4b": {
			"fp8-sglang-ppu-no-piecewise-96b30dc1": {"sglang", ppuRunnerSGLang, "96b30dc13593a244a5e59e84687309f53c375cfa", 5_190_052_872},
		},
		"qwen/qwen3-vl-32b-instruct": {
			"fp8-sglang-ppu-4bf2c2f3": {"sglang", ppuRunnerSGLang, "4bf2c2f39c37c0fede78bede4056e1f18cdf8109", 35_516_966_184},
		},
		"qwen/qwen2.5-vl-7b-instruct": {
			"int4-vllm-ppu-vision-536a3579": {"vllm", ppuRunnerVLLM, "536a35794df8831aa814970ee8f89eff577e7718", 6_923_972_384},
		},
		"qwen/qwen2.5-vl-72b-instruct": {
			"int4-vllm-ppu-vision-c8b87d4b": {"vllm", ppuRunnerVLLM, "c8b87d4b81f34b6a147577a310d7e75f0698f6c2", 43_004_946_024},
		},
		"qwen/qwen3-235b-a22b-instruct-2507": {
			"bf16-vllm-ppu-tp8-ac9c66cc": {"vllm", ppuRunnerVLLM, "ac9c66cc9b46af7306746a9250f23d47083d689e", 470_191_875_040},
		},
		"qwen/qwen3.5-397b-a17b": {
			"int4-vllm-ppu-tp4-df333de3": {"vllm", ppuRunnerVLLM, "df333de344e06b492063d1ca04e2d4ee878f7595", 235_708_083_992},
		},
		"poolside/laguna-s-2.1": {
			"int4-vllm-ppu-tp2-a6977d07": {"vllm", ppuRunnerVLLM, "a6977d070dded94c8776e29a94f7166ab1488e6c", 99_690_147_680},
		},
		"deepseek-ai/deepseek-v4-flash": {
			"fp8-vllm-ppu-pp4-60d8d707": {"vllm", ppuRunnerVLLM, "60d8d70770c6776ff598c94bb586a859a38244f1", 159_617_149_040},
		},
	}

	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		expectedSpecs, ok := want[model.ModelID]
		if !ok {
			continue
		}
		for specName, expected := range expectedSpecs {
			var found bool
			for _, spec := range model.Specs {
				if spec.Name != specName {
					continue
				}
				found = true
				if spec.Engine != expected.engine || spec.Image != expected.image || spec.GPUs.Vendor != "ppu" {
					t.Errorf("%s/%s runtime = engine %q, image %q, vendor %q; want %q/%q/ppu",
						model.ModelID, spec.Name, spec.Engine, spec.Image, spec.GPUs.Vendor, expected.engine, expected.image)
				}
				if spec.WeightBytes != expected.weightBytes {
					t.Errorf("%s/%s weight_bytes = %d, want %d", model.ModelID, spec.Name, spec.WeightBytes, expected.weightBytes)
				}
				revisionFound := false
				for _, source := range spec.Weights.Sources {
					if source.Revision == expected.revision {
						revisionFound = true
						break
					}
				}
				if !revisionFound {
					t.Errorf("%s/%s does not include tested revision %s", model.ModelID, spec.Name, expected.revision)
				}
			}
			if !found {
				t.Errorf("%s is missing PPU profile %s", model.ModelID, specName)
			}
			delete(expectedSpecs, specName)
		}
		for specName := range expectedSpecs {
			t.Errorf("model %s expects missing spec %s", model.ModelID, specName)
		}
		delete(want, model.ModelID)
	}
	for modelID := range want {
		t.Errorf("catalog is missing model %s", modelID)
	}
}
