package seed

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// TestEveryEmbeddedModelAnswersTheEstimationQuestion checks the invariant behind
// the measured KV footprint: a preset model either carries a measured
// kv_bytes_per_token or states that it keeps no KV cache, and never carries a
// nonsensical one.
//
// Coverage is reported rather than required while the catalog is being filled
// in (artifacts/archive/2026-09-13-vram-estimate/plan.md, slice C). The
// end state is that every deployable model is in one of the two groups, which is
// what the count below is for.
func TestEveryEmbeddedModelAnswersTheEstimationQuestion(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	sized, noCache, componentOnly, unmeasured := 0, 0, 0, 0
	for _, model := range catalog.Models {
		if len(model.Specs) == 0 {
			// Component-only rows (draft models, assistant adapters) are deployed
			// alongside a primary model and never own the KV pool.
			componentOnly++
			continue
		}
		for engine, bytes := range model.KVBytesPerToken {
			if bytes < 0 {
				t.Errorf("model %s kv_bytes_per_token[%s] is negative", model.ModelID, engine)
			}
		}
		if model.KVBytesPerToken["vllm"] > 0 && model.KVBytesPerToken["sglang"] > 0 {
			sized++
			continue
		}
		// No measurement. That is only acceptable as a deliberate statement that
		// the model keeps no token-indexed KV cache; anything else leaves the
		// deploy form unable to answer, which is the state the measured values
		// exist to end.
		if model.ModelConfig != nil && model.ModelConfig.KVCache != nil && !*model.ModelConfig.KVCache {
			noCache++
			continue
		}
		unmeasured++
	}
	t.Logf("measured=%d unmeasured=%d no-kv-cache=%d component-only=%d",
		sized, unmeasured, noCache, componentOnly)
	if unmeasured > 0 {
		t.Logf("COVERAGE GAP: %d deployable models carry no measured KV footprint", unmeasured)
	}
}

func TestHybridCatalogKVValuesUseAttentionLayersForBothEngines(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	attention := map[string]bool{
		"attention": true, "full_attention": true,
		"sliding_attention": true, "global_attention": true,
	}
	checked := 0
	for _, model := range catalog.Models {
		// Missing measurements are reported by the estimation coverage test.
		// Do not require an invented KV footprint for a newly added model:
		// sparse indexers and engine-specific cache layouts need measurement.
		if len(model.KVBytesPerToken) == 0 {
			continue
		}
		cfg := model.ModelConfig
		if cfg == nil || len(cfg.LayerTypes) == 0 {
			continue
		}
		hybrid := false
		layers := 0
		for _, kind := range cfg.LayerTypes {
			if kind == "linear_attention" || kind == "conv" {
				hybrid = true
			}
			if attention[kind] {
				layers++
			}
		}
		if !hybrid || cfg.IsMLA() || layers == 0 || cfg.EffectiveKVHeads() <= 0 || cfg.EffectiveHeadDim() <= 0 {
			continue
		}
		checked++
		want := int64(layers * cfg.EffectiveKVHeads() * cfg.EffectiveHeadDim() * 2 * 2)
		if model.KVBytesPerToken["vllm"] != want || model.KVBytesPerToken["sglang"] != want {
			t.Errorf("%s: hybrid KV values = vllm=%d sglang=%d, want %d (%d attention layers)",
				model.ModelID, model.KVBytesPerToken["vllm"], model.KVBytesPerToken["sglang"], want, layers)
		}
	}
	if checked == 0 {
		t.Fatal("no hybrid catalog models were checked")
	}
}

func TestEmbeddedModelConfigAnswersTheSameQuestionAsTheRepository(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, model := range catalog.Models {
		if model.ModelConfig == nil || len(model.Specs) == 0 {
			continue
		}
		// The block travels to the estimator as JSON through the store, not as
		// the YAML the catalog author wrote.
		encoded, err := json.Marshal(model.ModelConfig)
		if err != nil {
			t.Errorf("model %s model_config is not encodable: %v", model.ModelID, err)
			continue
		}
		roundTripped, err := modelconfig.Parse(bytes.NewReader(encoded))
		if err != nil {
			t.Errorf("model %s model_config does not survive the store round trip: %v", model.ModelID, err)
			continue
		}
		// The config is the metadata a deployment still reads for TP
		// divisibility, context length and encoder sizing; it round-trips as the
		// same facts or those break silently.
		if !reflect.DeepEqual(roundTripped, model.ModelConfig) {
			t.Errorf("model %s: model_config does not survive the store round trip", model.ModelID)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no model_config was checked")
	}
}
