package planning

import (
	"encoding/json"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func runningPD() *PDPlacement {
	return &PDPlacement{Mode: "vllm-nixl", Roles: map[string]PDRole{
		"prefill": {TP: 1, Units: []PDUnit{{NodeUUID: "a"}}},
		"decode":  {TP: 2, Units: []PDUnit{{NodeUUID: "b"}}},
	}}
}

func TestApplyPDUpdateScalesUnitsAndKeepsWhatItWasNotAsked(t *testing.T) {
	current := runningPD()
	fail := "fail"
	applied, err := ApplyPDUpdate(current, "vllm", PDUpdate{
		Roles:    map[string]PDRole{"prefill": {Units: []PDUnit{{NodeUUID: "a"}, {NodeUUID: "c"}}}},
		Fallback: &fail,
	})
	if err != nil {
		t.Fatal(err)
	}
	next := applied.Placement
	if len(next.Units("prefill")) != 2 || next.TP("prefill") != 1 {
		t.Fatalf("prefill after scale-up = %+v", next.Roles["prefill"])
	}
	if len(next.Units("decode")) != 1 || next.TP("decode") != 2 || next.Fallback != "fail" {
		t.Fatalf("untouched parts changed: %+v", next)
	}
	if len(current.Units("prefill")) != 1 || current.Fallback != "" {
		t.Fatalf("the current placement was modified: %+v", current)
	}

	shrunk, err := ApplyPDUpdate(current, "vllm", PDUpdate{Roles: map[string]PDRole{"prefill": {Units: []PDUnit{}}}})
	if err != nil || len(shrunk.Placement.Units("prefill")) != 0 {
		t.Fatalf("prefill to zero = %+v, %v", shrunk, err)
	}
}

func TestApplyPDUpdateRefusesWhatIsFixedAtLaunch(t *testing.T) {
	cases := map[string]PDUpdate{
		"mode":            {Mode: "sglang-nixl"},
		"kv load failure": {KVLoadFailure: "recompute"},
		"tp":              {Roles: map[string]PDRole{"decode": {TP: 4, Units: []PDUnit{{NodeUUID: "b"}}}}},
		"pp":              {Roles: map[string]PDRole{"decode": {PP: 2, Units: []PDUnit{{NodeUUID: "b"}}}}},
	}
	for name, update := range cases {
		if _, err := ApplyPDUpdate(runningPD(), "vllm", update); productCode(err) != messages.ErrorPDImmutable {
			t.Errorf("%s: error = %v, want %s", name, err, messages.ErrorPDImmutable)
		}
	}
	// Restating what it already is, is not a change.
	same := PDUpdate{Mode: "vllm-nixl", KVLoadFailure: "fail", Roles: map[string]PDRole{"decode": {TP: 2, PP: 1, Units: []PDUnit{{NodeUUID: "b"}}}}}
	if _, err := ApplyPDUpdate(runningPD(), "vllm", same); err != nil {
		t.Fatalf("restated launch settings: %v", err)
	}
	if _, err := ApplyPDUpdate(runningPD(), "vllm", PDUpdate{Roles: map[string]PDRole{"decode": {Units: []PDUnit{}}}}); productCode(err) != messages.ErrorPDRoleUnitsEmpty {
		t.Fatalf("decode to zero: %v", err)
	}
	if _, err := ApplyPDUpdate(nil, "vllm", PDUpdate{}); productCode(err) != messages.ErrorPDNotPD {
		t.Fatalf("ordinary deployment: %v", err)
	}
}

func TestWithPDKeepsTheOtherPlacementKeys(t *testing.T) {
	encoded, err := WithPD(`{"gpus":[3],"pd":{"mode":"old"}}`, runningPD())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(encoded), &doc); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(doc["gpus"]) != "[3]" || doc["pd"].(map[string]any)["mode"] != "vllm-nixl" {
		t.Fatalf("placement = %s", encoded)
	}
	if _, err := WithPD(`{"pd":`, runningPD()); err == nil {
		t.Fatal("an unreadable stored placement was overwritten")
	}
}
