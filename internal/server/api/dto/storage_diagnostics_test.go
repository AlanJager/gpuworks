package dto

import (
	"encoding/json"
	"testing"
)

func TestStorageProbeDiagnosticWireCompatibility(t *testing.T) {
	const current = `{"node_uuid":"n1","node_name":"GPU 1","ok":false,"target":"nfs.example:/models","checked_at":"2026-11-04T08:00:00Z","reason":"mount_failed","readable":false,"writable":false}`
	var result StorageProbeResult
	if err := json.Unmarshal([]byte(current), &result); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	const encodedWire = `{"node_uuid":"n1","node_name":"GPU 1","target":"nfs.example:/models","checked_at":"2026-11-04T08:00:00Z","ok":false,"reason":"mount_failed","readable":false,"writable":false}`
	if string(encoded) != encodedWire {
		t.Fatalf("wire contract = %s, want %s", encoded, encodedWire)
	}
	var wire map[string]any
	_ = json.Unmarshal(encoded, &wire)
	if wire["target"] != "nfs.example:/models" || wire["checked_at"] != "2026-11-04T08:00:00Z" {
		t.Fatalf("evidence was not retained: %s", encoded)
	}
	const old = `{"node_uuid":"n1","node_name":"GPU 1","ok":true,"readable":true,"writable":true}`
	var legacy StorageProbeResult
	if err := json.Unmarshal([]byte(old), &legacy); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != old {
		t.Fatalf("legacy observation fabricated metadata: %s", encoded)
	}
}
