package deploy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPoolingTaskArgsUseExplicitContractAndVersion(t *testing.T) {
	for _, tc := range []struct {
		name, engine, version, operations string
		template, extra, want             []string
	}{
		{name: "current embedding", engine: "vllm", version: "0.30.0", operations: `[{"type":"openai.embeddings"}]`, want: []string{"--runner", "pooling"}},
		{name: "PPU reranker", engine: "vllm", version: "0.23.0", operations: `[{"type":"openai.rerank"}]`, want: []string{"--runner", "pooling"}},
		{name: "first runner release", engine: "vllm", version: "0.10.0", operations: `[{"type":"openai.rerank"}]`, want: []string{"--runner", "pooling"}},
		{name: "legacy score", engine: "vllm", version: "0.9.0", operations: `[{"type":"openai.rerank"}]`, want: []string{"--task", "score"}},
		{name: "legacy embed", engine: "vllm", version: "0.8.5", operations: `[{"type":"openai.embeddings"}]`, want: []string{"--task", "embed"}},
		{name: "unknown release", engine: "vllm", operations: `[{"type":"openai.rerank"}]`},
		{name: "unknown old release", engine: "vllm", version: "0.7.0", operations: `[{"type":"openai.rerank"}]`},
		{name: "other engine", engine: "sglang", version: "0.23.0", operations: `[{"type":"openai.embeddings"}]`},
		{name: "chat contract", engine: "vllm", version: "0.23.0", operations: `[{"type":"openai.chat"}]`},
		{name: "mixed contract", engine: "vllm", version: "0.23.0", operations: `[{"type":"openai.chat"},{"type":"openai.rerank"}]`},
		{name: "two pooling operations ambiguous on old releases", engine: "vllm", version: "0.9.0", operations: `[{"type":"openai.embeddings"},{"type":"openai.rerank"}]`},
		{name: "no operation", engine: "vllm", version: "0.23.0", operations: `[]`},
		{name: "explicit user runner", engine: "vllm", version: "0.23.0", operations: `[{"type":"openai.rerank"}]`, extra: []string{"--runner=generate"}},
		{name: "explicit template task", engine: "vllm", version: "0.23.0", operations: `[{"type":"openai.rerank"}]`, template: []string{"--task", "auto"}},
		{name: "omni launch", engine: "vllm", version: "0.30.0", operations: `[{"type":"openai.embeddings"}]`, extra: []string{"--omni"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := json.RawMessage(`{"operations":` + tc.operations + `}`)
			got := PoolingTaskArgs(tc.engine, tc.version, contract, tc.template, tc.extra)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("args=%v, want %v", got, tc.want)
			}
		})
	}
	if got := PoolingTaskArgs("vllm", "0.23.0", json.RawMessage(`bad JSON`)); len(got) != 0 {
		t.Fatalf("malformed contract gained task: %v", got)
	}
}
