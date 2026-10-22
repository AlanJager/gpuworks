package dto

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// api.request_stats on the wire: a bare stat name, or {to, scale} when the
// value is scaled, read back in either form.
func TestAPIContractRequestStatsWire(t *testing.T) {
	contract := APIContract{
		Operations: []APIOperation{},
		RequestStats: inferenceapi.MetricMap{
			"usage.prompt_tokens_details.cached_tokens": {To: "cached_tokens"},
			"timings.queue_ms":                          {To: "queue_seconds", Scale: 0.001},
		},
	}
	wire, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"operations":[],"request_stats":{"timings.queue_ms":{"to":"queue_seconds","scale":0.001},"usage.prompt_tokens_details.cached_tokens":"cached_tokens"}}`
	if string(wire) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", wire, want)
	}
	var back APIContract
	if err := json.Unmarshal([]byte(want), &back); err != nil {
		t.Fatal(err)
	}
	if back.RequestStats["timings.queue_ms"] != (inferenceapi.MetricTarget{To: "queue_seconds", Scale: 0.001}) ||
		back.RequestStats["usage.prompt_tokens_details.cached_tokens"] != (inferenceapi.MetricTarget{To: "cached_tokens"}) {
		t.Fatalf("round trip = %+v", back.RequestStats)
	}
	if err := json.Unmarshal([]byte(`{"operations":[],"request_stats":{"x":{"to":"y","factor":2}}}`), &back); err == nil {
		t.Fatal("an unknown key in a stat target must be refused")
	}
	empty, _ := json.Marshal(APIContract{Operations: []APIOperation{}})
	if string(empty) != `{"operations":[]}` {
		t.Fatalf("a contract without request stats = %s", empty)
	}
}
