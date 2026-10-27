package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestCreateBenchmarkRejectsEmptyExplicitTokenizerReference(t *testing.T) {
	a := newBenchmarkAdmission(t, "benchmark_empty_explicit_tokenizer")
	dep, node := a.managedTarget(t, "tokenizer-admission")
	// Node-origin creation uses the real bench.probe wire response, without
	// executing a run or reaching any public model/tokenizer repository.
	a.handlers.d.CallNode = func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
		input, ok := params.(map[string]string)
		if uuid != node.UUID || method != proto.RPCBenchProbe || !ok || input["url"] != "https://model.example" {
			return nil, fmt.Errorf("unexpected node probe %s %s", uuid, method)
		}
		return json.RawMessage(`{"data":[{"id":"m"}]}`), nil
	}
	for _, target := range []string{"instance", "external"} {
		for _, kind := range []string{"defaults", "random", "multiturn", "agentic"} {
			for _, source := range []string{"huggingface", "modelscope", "upload"} {
				for _, reference := range []string{"omitted", "", " \t\n "} {
					t.Run(fmt.Sprintf("%s/%s/%s/%q", target, kind, source, reference), func(t *testing.T) {
						body := map[string]any{"name": "invalid tokenizer", "target_kind": target, "deployment_id": dep.ID, "base_url": "https://model.example", "model": "m", "load_origin": node.UUID, "tokenizer_source": source, "allow_conflict": true}
						if kind != "defaults" {
							body["dataset"] = map[string]any{"kind": kind}
						}
						if reference != "omitted" {
							body["tokenizer_ref"] = reference
						}
						before, err := a.store.ListBenchmarkRuns()
						if err != nil {
							t.Fatal(err)
						}
						raw, _ := json.Marshal(body)
						rec := a.create(t, string(raw))
						var wire struct {
							Code   string `json:"code"`
							Params struct {
								Field string `json:"field"`
							} `json:"params"`
						}
						if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
							t.Fatal(err)
						}
						if rec.Code != http.StatusBadRequest || wire.Code != ErrorBenchmarkParameterInvalid || wire.Params.Field != "tokenizer_ref" {
							t.Errorf("missing tokenizer admitted or wrong error: status=%d body=%s", rec.Code, rec.Body.String())
						}
						after, err := a.store.ListBenchmarkRuns()
						if err != nil {
							t.Fatal(err)
						}
						if len(after) != len(before) {
							t.Errorf("invalid request persisted %d new runs", len(after)-len(before))
						}
					})
				}
			}
		}
	}
}

func TestCreateBenchmarkExplicitTokenizerReferencesArePreserved(t *testing.T) {
	a := newBenchmarkAdmission(t, "benchmark_explicit_tokenizer_preserved")
	dep, node := a.managedTarget(t, "explicit-tokenizer")
	for _, tc := range []struct{ source, reference string }{
		{"huggingface", "hf://org/selected-model"},
		{"modelscope", "ms://org/selected-model"},
		{"upload", "/var/lib/gpuworks/tokenizer-selected.json"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			body := fmt.Sprintf(`{"name":"explicit tokenizer","target_kind":"instance","deployment_id":%d,"load_origin":%q,"tokenizer_source":%q,"tokenizer_ref":%q,"allow_conflict":true}`, dep.ID, node.UUID, tc.source, " \t"+tc.reference+"\n ")
			rec := a.create(t, body)
			var created benchmarkDTO
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			persisted, err := a.store.GetBenchmarkRun(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if created.TokenizerRef != tc.reference || persisted.TokenizerRef != tc.reference {
				t.Fatalf("selected reference changed: wire=%q persisted=%q", created.TokenizerRef, persisted.TokenizerRef)
			}
		})
	}
}
