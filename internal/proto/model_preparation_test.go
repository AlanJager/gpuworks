package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelPreparationWireCompatibility(t *testing.T) {
	const wire = `{"key":"f7","state":"downloading","bytes_total":100,"bytes_done":40,"rate_bytes_per_s":10,"transfer_id":"intent","preparation":{"attempt":"snapshot","phase":"copying","total_known":true,"idle_seconds":2,"stalled":true,"rate_stable":true}}`
	var report ModelFileStatus
	if err := json.Unmarshal([]byte(wire), &report); err != nil {
		t.Fatal(err)
	}
	if report.Preparation == nil || report.Preparation.Phase != "copying" || report.Preparation.IdleSeconds != 2 || !report.Preparation.RateStable || report.TransferID != "intent" {
		t.Fatal(report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var expected, actual map[string]any
	json.Unmarshal([]byte(wire), &expected)
	json.Unmarshal(encoded, &actual)
	if len(expected) != len(actual) {
		t.Fatalf("wire keys: %s", encoded)
	}
	for key, value := range expected {
		a, _ := json.Marshal(actual[key])
		b, _ := json.Marshal(value)
		if string(a) != string(b) {
			t.Fatalf("%s: %s != %s", key, a, b)
		}
	}
	var old ModelFileStatus
	if err := json.Unmarshal([]byte(`{"key":"f7","state":"downloading","bytes_total":100,"bytes_done":40,"rate_bytes_per_s":10}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Preparation != nil {
		t.Fatal("old report synthesized a phase")
	}
	encoded, _ = json.Marshal(old)
	if strings.Contains(string(encoded), "preparation") {
		t.Fatalf("optional field leaked: %s", encoded)
	}
}
