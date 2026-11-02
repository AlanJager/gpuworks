package seed

import (
	"slices"
	"testing"
)

func TestGemma426A3UsesCheckpointTemplateAndEagerMode(t *testing.T) {
	_, spec := ppuSpec(t, "google/gemma-4-26b-a4b-it", "bf16-vllm-ascend-a3-d8ff96c2")

	if !slices.Contains(spec.Args, "--enforce-eager") {
		t.Error("A3 Gemma-4-26B must run without graph capture")
	}
	for index := 0; index+1 < len(spec.Args); index++ {
		if spec.Args[index] != "--chat-template" {
			continue
		}
		if got := spec.Args[index+1]; got != "/model/chat_template.jinja" {
			t.Fatalf("A3 Gemma-4-26B chat template = %q, want the checkpoint-local file", got)
		}
		return
	}
	t.Fatal("A3 Gemma-4-26B does not select a chat template")
}
