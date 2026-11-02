package dto

import (
	"encoding/json"
	"strings"
	"testing"
)

// PUT /deployments/{id}/placement-mode takes exactly one key, mode.
func TestPlacementModeRequestWire(t *testing.T) {
	for _, mode := range []string{"auto", "manual"} {
		raw := marshalString(t, PlacementModeRequest{Mode: mode})
		if want := `{"mode":"` + mode + `"}`; raw != want {
			t.Fatalf("request = %s, want %s", raw, want)
		}
		var back PlacementModeRequest
		if err := json.Unmarshal([]byte(raw), &back); err != nil || back.Mode != mode {
			t.Fatalf("round trip of %s = %+v %v", raw, back, err)
		}
	}
	var req PlacementModeRequest
	if err := json.Unmarshal([]byte(`{"mode":5}`), &req); err == nil {
		t.Fatal("a numeric mode was accepted")
	}
	// A body without the key decodes to the empty mode, which the handler refuses.
	if err := json.Unmarshal([]byte(`{}`), &req); err != nil || req.Mode != "" {
		t.Fatalf("empty body = %+v %v", req, err)
	}
}

// The evaluate response says the server picked the nodes only when it did:
// auto_placed is absent otherwise, and true when set.
func TestEvaluateResponseAutoPlacedWire(t *testing.T) {
	keys := func(r EvaluateResponse) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(marshalString(t, r)), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if _, present := keys(EvaluateResponse{OK: true})["auto_placed"]; present {
		t.Fatal("auto_placed must be omitted when the server picked nothing")
	}
	if got := string(keys(EvaluateResponse{OK: true, AutoPlaced: true})["auto_placed"]); got != `true` {
		t.Fatalf("auto_placed = %s, want true", got)
	}
	raw := marshalString(t, EvaluateResponse{OK: true, AutoPlaced: true})
	if !strings.Contains(raw, `"auto_placed":true`) {
		t.Fatalf("wire lacks the literal key: %s", raw)
	}
	var back EvaluateResponse
	if err := json.Unmarshal([]byte(`{"ok":true,"auto_placed":true}`), &back); err != nil || !back.AutoPlaced {
		t.Fatalf("decoded = %+v %v", back, err)
	}
	back = EvaluateResponse{}
	if err := json.Unmarshal([]byte(`{"ok":true}`), &back); err != nil || back.AutoPlaced {
		t.Fatalf("absent key decoded = %+v %v", back, err)
	}
}

// The deployment carries placement_mode only for a multi-node or PD
// deployment; a single-node one has no such key.
func TestDeploymentPlacementModeWire(t *testing.T) {
	keys := func(d Deployment) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(marshalString(t, d)), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, mode := range []string{"auto", "manual"} {
		if got := string(keys(Deployment{PlacementMode: mode})["placement_mode"]); got != `"`+mode+`"` {
			t.Fatalf("placement_mode = %s, want %q", got, mode)
		}
	}
	if _, present := keys(Deployment{})["placement_mode"]; present {
		t.Fatal("a deployment without a placement mode must not write the key")
	}
	var back Deployment
	if err := json.Unmarshal([]byte(`{"placement_mode":"auto"}`), &back); err != nil || back.PlacementMode != "auto" {
		t.Fatalf("decoded = %+v %v", back.PlacementMode, err)
	}
}
