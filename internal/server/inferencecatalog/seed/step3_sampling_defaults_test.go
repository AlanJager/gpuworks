package seed

import "testing"

func TestStep3AscendProfilesIgnoreInvalidTopKFromModelGenerationConfig(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"bf16-sglang-ascend-d02b6a67":    false,
		"bf16-sglang-ascend-a3-d02b6a67": false,
	}
	for _, model := range catalog.Models {
		if model.ModelID != "stepfun-ai/step3-vl-10b" {
			continue
		}
		for _, spec := range model.Specs {
			if _, ok := want[spec.Name]; !ok {
				continue
			}
			want[spec.Name] = containsArgumentPair(spec.Args, "--sampling-defaults", "openai")
		}
	}
	for spec, ok := range want {
		if !ok {
			t.Errorf("%s must use OpenAI sampling defaults because the checkpoint declares top_k=0, which SGLang rejects", spec)
		}
	}
}
