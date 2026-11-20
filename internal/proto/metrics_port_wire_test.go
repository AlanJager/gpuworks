package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// The metrics port rides in the instance's desired state under "metrics_port".
// A desired state stored before the field existed reads as no metrics port (the
// API port is scraped), a launch without one writes no key at all, and a launch
// with one round-trips unchanged: the agent, which restarts and reconnects with
// the stored desired state, scrapes the same port for the instance's whole life.
func TestInstanceDesiredStateMetricsPortWireJSON(t *testing.T) {
	var old InstanceDesiredState
	if err := json.Unmarshal([]byte(`{"launch":{"argv":[],"model_mount":""},"uuid":"u","ports":[30000],"api_port":30000}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.APIPort != 30000 || old.MetricsPort != 0 {
		t.Fatalf("stored before the field: api %d metrics %d", old.APIPort, old.MetricsPort)
	}
	encoded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "metrics_port") {
		t.Fatalf("a launch with no metrics port wrote one: %s", encoded)
	}

	var own InstanceDesiredState
	if err := json.Unmarshal([]byte(`{"launch":{"argv":[],"model_mount":""},"uuid":"u","ports":[30000,30001],"api_port":30000,"metrics_port":30001}`), &own); err != nil {
		t.Fatal(err)
	}
	if own.MetricsPort != 30001 || len(own.Ports) != 2 {
		t.Fatalf("decoded %+v", own)
	}
	encoded, err = json.Marshal(own)
	if err != nil {
		t.Fatal(err)
	}
	var again InstanceDesiredState
	if err := json.Unmarshal(encoded, &again); err != nil || again.MetricsPort != 30001 || !strings.Contains(string(encoded), `"metrics_port":30001`) {
		t.Fatalf("round trip %s: %+v, %v", encoded, again, err)
	}
}
