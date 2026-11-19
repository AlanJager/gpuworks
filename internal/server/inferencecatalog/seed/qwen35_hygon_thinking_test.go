package seed

import (
	"slices"
	"strings"
	"testing"
)

// Qwen3.5-35B-A3B thinks before it answers. On the Hygon K100_AI spec a
// one-line greeting spent about 1200 tokens in message.reasoning first, so a
// request capped at 512 or 1536 tokens ended with finish_reason=length and no
// content at all (GWKS-373; measured on the dtk26.04-vllm0.18.1-r2 runner,
// where the same request with enable_thinking=false answered in three
// tokens). The spec turns thinking off by default and keeps the reasoning
// parser, so a request that asks for thinking
// (chat_template_kwargs.enable_thinking=true) still gets it, parsed.
//
// Only this spec is pinned: it is the one measured. The family's other
// vendor specs that still think by default are not changed here.
func TestQwen35HygonChatAnswersWithoutThinkingByDefault(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	const modelID, specName = "Qwen/Qwen3.5-35B-A3B", "int8-vllm-hygon-6f448b0f"
	for _, model := range catalog.Models {
		if !strings.EqualFold(model.ModelID, modelID) {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != specName {
				continue
			}
			if !slices.Contains(spec.Args, "--default-chat-template-kwargs.enable_thinking=false") {
				t.Errorf("%s/%s does not turn thinking off by default: %v", modelID, specName, spec.Args)
			}
			parser := slices.Index(spec.Args, "--reasoning-parser")
			if parser < 0 || parser+1 >= len(spec.Args) || spec.Args[parser+1] != "qwen3" {
				t.Errorf("%s/%s lost its reasoning parser, which a request that opts into thinking needs: %v", modelID, specName, spec.Args)
			}
			return
		}
	}
	t.Fatalf("%s has no spec %s", modelID, specName)
}
