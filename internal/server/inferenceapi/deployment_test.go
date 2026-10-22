package inferenceapi

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelContractUsesOnlyCurrentOperations(t *testing.T) {
	tests := []struct {
		name       string
		operations string
		want       []string
	}{
		{name: "valid", operations: `[{"type":" OPENAI.CHAT "}]`, want: []string{OperationChat}},
		{name: "empty array", operations: `[]`},
		{name: "missing"},
		{name: "malformed", operations: `{not-json`},
		{name: "unsupported", operations: `[{"type":"openai.invalid"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ModelContract(&store.Model{Operations: tt.operations})
			if len(got.Operations) != len(tt.want) {
				t.Fatalf("operations=%+v want types=%v", got.Operations, tt.want)
			}
			for i, want := range tt.want {
				if got.Operations[i].Type != want {
					t.Fatalf("operations[%d]=%q want %q", i, got.Operations[i].Type, want)
				}
			}
		})
	}
}

func TestModelContractNormalizesConfiguredDefaults(t *testing.T) {
	got := ModelContract(&store.Model{Operations: `[{"type":"openai.rerank"}]`})
	if len(got.Operations) != 1 || got.Operations[0].Endpoint != "/v1/rerank" || got.Operations[0].Mode != "sync" {
		t.Fatalf("normalized contract=%+v", got)
	}
}
