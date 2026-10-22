package report

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestReportContextRetainsFactsWithOptionalJSON(t *testing.T) {
	variants := []struct {
		name string
		raw  json.RawMessage
	}{
		{"nil", nil}, {"empty", json.RawMessage("")}, {"blank", json.RawMessage(" \t\r\n ")},
		{"null", json.RawMessage("null")}, {"valid", json.RawMessage(`{"concurrency":[1]}`)},
	}
	for _, verdict := range variants {
		if verdict.name == "valid" {
			verdict.raw = json.RawMessage(`{"kind":"grid_done"}`)
		}
		for _, search := range variants {
			t.Run("verdict_"+verdict.name+"/search_"+search.name, func(t *testing.T) {
				run := Run{ID: 9, Name: "canceled report", State: "canceled", Mode: "grid", Model: "test-model",
					Verdict: verdict.raw, Search: search.raw, Dataset: json.RawMessage(`{"kind":"random","seed":42}`),
					TargetSnapshot: json.RawMessage(`{"target_kind":"instance","engine":"vllm","tensor_parallel":1}`),
					Points:         []Point{{Seq: 1, State: "canceled", Concurrency: 1}},
				}
				row := csvPoint(t, run)
				var context map[string]json.RawMessage
				if err := json.Unmarshal([]byte(row["report_context_json"]), &context); err != nil {
					t.Fatalf("context JSON invalid: %v", err)
				}
				for key, want := range map[string]string{
					"run_id": "9", "model": `"test-model"`, "dataset": `{"kind":"random","seed":42}`,
					"target_snapshot":   `{"target_kind":"instance","engine":"vllm","tensor_parallel":1}`,
					"persisted_verdict": expectedOptionalJSON(verdict.raw), "search": expectedOptionalJSON(search.raw),
				} {
					var gotValue, wantValue any
					if err := json.Unmarshal(context[key], &gotValue); err != nil {
						t.Fatalf("%s invalid/missing: %v", key, err)
					}
					if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(gotValue, wantValue) {
						t.Fatalf("%s=%s want %s", key, context[key], want)
					}
				}
			})
		}
	}
}

func expectedOptionalJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == " \t\r\n " {
		return "null"
	}
	return string(raw)
}

func TestOptionalContextJSONPreservesMalformedNonemptyJSON(t *testing.T) {
	for _, input := range []string{`{`, `not-json`, ` {"kind": `} {
		raw := json.RawMessage(input)
		got := optionalContextJSON(raw)
		if string(got) != input || json.Valid(got) {
			t.Fatalf("malformed nonempty JSON was rewritten: %q -> %q", input, got)
		}
	}
}
