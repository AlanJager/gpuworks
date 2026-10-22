package dto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAutomaticPlacementRequestAndResponseWireContract(t *testing.T) {
	request := DeployRequest{
		ModelID: 7, PlacementMode: "auto", TargetVendor: "nvidia",
		NodeUUID: "node-confirmed", Placement: map[string]any{"gpus": []int{2, 4}, "tp": 2},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	wantRequestFields := []string{
		`"placement_mode":"auto"`, `"target_vendor":"nvidia"`,
		`"node_uuid":"node-confirmed"`, `"placement":{"gpus":[2,4],"tp":2}`,
	}
	for _, field := range wantRequestFields {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("request missing literal wire field %s: %s", field, raw)
		}
	}
	var roundRequest DeployRequest
	if err := json.Unmarshal(raw, &roundRequest); err != nil {
		t.Fatal(err)
	}
	if roundRequest.PlacementMode != "auto" || roundRequest.TargetVendor != "nvidia" || roundRequest.NodeUUID != "node-confirmed" {
		t.Fatalf("request round trip = %+v", roundRequest)
	}

	response := EvaluateResponse{
		OK: true, PlanScope: "per_replica",
		SelectedNode: &PlacementNode{UUID: "node-confirmed", Name: "gpu-a", Reasons: []ScheduleReason{{Code: "packing", Score: 10}}},
		Candidates: []PlacementCandidate{
			{NodeUUID: "node-confirmed", NodeName: "gpu-a", OK: true, Reason: ScheduleReason{Code: "filters_passed"}},
			{NodeUUID: "node-rejected", OK: false, Reason: ScheduleReason{Code: "node_label_mismatch", Params: map[string]any{"detail": "pool"}}},
		},
	}
	raw, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	literal := string(raw)
	for _, field := range []string{
		`"selected_node":{"uuid":"node-confirmed","name":"gpu-a","reasons":[{"code":"packing","score":10}]}`,
		`"candidates":[{"node_uuid":"node-confirmed","node_name":"gpu-a","ok":true,"reason":{"code":"filters_passed"}},{"node_uuid":"node-rejected","ok":false,"reason":{"code":"node_label_mismatch","params":{"detail":"pool"}}}]`,
	} {
		if !strings.Contains(literal, field) {
			t.Fatalf("response missing literal wire field %s: %s", field, literal)
		}
	}
	var roundResponse EvaluateResponse
	if err := json.Unmarshal(raw, &roundResponse); err != nil {
		t.Fatal(err)
	}
	if roundResponse.SelectedNode == nil || roundResponse.SelectedNode.UUID != "node-confirmed" || len(roundResponse.Candidates) != 2 || roundResponse.Candidates[1].Reason.Code != "node_label_mismatch" {
		t.Fatalf("response round trip = %+v", roundResponse)
	}
}
