package proto

import (
	"encoding/json"
	"testing"
)

func TestConnectionDiagnosticsUsesExistingDoctorWire(t *testing.T) {
	const wire = `{"report":"","facts":[{"code":"connection.source","value":"agent_runtime"},{"code":"connection.control","value":"connected"},{"code":"connection.data_lane","value":"pending"}]}`
	var report DoctorReport
	if err := json.Unmarshal([]byte(wire), &report); err != nil {
		t.Fatal(err)
	}
	if got := ConnectionValue(report, "connection.data_lane"); got != "pending" {
		t.Fatalf("data lane = %q", got)
	}
	if ConnectionValue(report, "connection.control") != "connected" || ConnectionValue(report, "control") != "" {
		t.Fatal("fact names must match exactly")
	}
	raw, err := json.Marshal(report)
	if err != nil || string(raw) != wire {
		t.Fatalf("wire = %s, err = %v", raw, err)
	}
	report.Facts = append(report.Facts, DoctorFact{Code: "connection.control", Value: "connected"})
	if ConnectionValue(report, "connection.control") != "" {
		t.Fatal("repeated facts must not be treated as an authoritative unique value")
	}
	if RPCNodeConnectionDiagnostics != "node.connection_diagnostics" || CapConnectionDiagnostics != "node.connection_diagnostics" || RPCNodeConnectionDiagnostics == RPCNodeDoctor {
		t.Fatal("connection-only protocol identity changed")
	}
}
