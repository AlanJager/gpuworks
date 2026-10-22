package dto

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStorageNodeOverrideWireThreeStates(t *testing.T) {
	for _, tc := range []struct {
		wire    string
		present bool
		value   *int64
	}{
		{`{"node_uuids":["n1"]}`, false, nil},
		{`{"node_uuids":["n1"],"cache_max_bytes":null}`, true, nil},
		{`{"node_uuids":["n1"],"cache_max_bytes":0}`, true, new(int64)},
	} {
		var req StorageNodeOverridesRequest
		if err := json.Unmarshal([]byte(tc.wire), &req); err != nil {
			t.Fatal(err)
		}
		if req.CacheMaxBytes.Present != tc.present || (req.CacheMaxBytes.Value == nil) != (tc.value == nil) {
			t.Fatalf("tri-state lost: %+v", req)
		}
		if tc.value != nil && *req.CacheMaxBytes.Value != *tc.value {
			t.Fatal("zero changed")
		}
	}
	for _, wire := range []string{`{"cache_max_bytes":9223372036854775808}`, `{"cache_max_bytes":1.2}`, `{"cache_max_bytes":"0"}`, `{"cache_path":42}`} {
		if err := json.Unmarshal([]byte(wire), &StorageNodeOverridesRequest{}); err == nil {
			t.Fatalf("invalid accepted: %s", wire)
		}
	}
	var req StorageNodeOverridesRequest
	if err := json.Unmarshal([]byte(`{"cache_path":null}`), &req); err != nil || !req.CachePath.Present || req.CachePath.Value != nil {
		t.Fatalf("null path: %+v %v", req, err)
	}
}

func TestStorageOverrideAndRevisionWire(t *testing.T) {
	const wire = `{"override_only":true,"node_uuid":"n1","cache_max_bytes":0}`
	var target StorageTarget
	if err := json.Unmarshal([]byte(wire), &target); err != nil {
		t.Fatal(err)
	}
	if !target.OverrideOnly || target.CacheMaxBytes == nil || *target.CacheMaxBytes != 0 {
		t.Fatalf("decoded: %+v", target)
	}
	encoded, err := json.Marshal(target)
	if err != nil || string(encoded) != wire {
		t.Fatalf("round trip %s %v", encoded, err)
	}
	const patch = `{"expected_updated_at":"2026-10-16T12:00:00.123456789Z","cache_max_bytes":0}`
	var req StorageRequest
	if err := json.Unmarshal([]byte(patch), &req); err != nil {
		t.Fatal(err)
	}
	if req.ExpectedUpdatedAt == nil || req.ExpectedUpdatedAt.Format(time.RFC3339Nano) != "2026-10-16T12:00:00.123456789Z" {
		t.Fatalf("revision: %+v", req)
	}
	encoded, err = json.Marshal(req)
	if err != nil || string(encoded) != patch {
		t.Fatalf("round trip %s %v", encoded, err)
	}
}
