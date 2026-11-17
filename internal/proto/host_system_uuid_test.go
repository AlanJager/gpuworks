package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// host_system_uuid rides in the hello; agents that predate it, or cannot
// read it, send none and the key is omitted.
func TestHelloHostSystemUUIDWireJSON(t *testing.T) {
	raw, err := json.Marshal(Hello{ProtoMin: 1, ProtoMax: 2, Version: "v1", HostSystemUUID: "4c4c4544-0042-3510-8051-b4c04f4e3432"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"proto_min":1,"proto_max":2,"version":"v1","host_system_uuid":"4c4c4544-0042-3510-8051-b4c04f4e3432"}`
	if string(raw) != want {
		t.Fatalf("wire = %s, want %s", raw, want)
	}
	raw, _ = json.Marshal(Hello{ProtoMin: 1, ProtoMax: 2, Version: "v1"})
	if strings.Contains(string(raw), "host_system_uuid") {
		t.Fatalf("empty host_system_uuid was not omitted: %s", raw)
	}
	var got Hello
	if err := json.Unmarshal([]byte(want), &got); err != nil || got.HostSystemUUID != "4c4c4544-0042-3510-8051-b4c04f4e3432" {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	// An old agent's hello decodes with the field empty.
	var old Hello
	if err := json.Unmarshal([]byte(`{"proto_min":1,"proto_max":2,"version":"old"}`), &old); err != nil || old.HostSystemUUID != "" {
		t.Fatalf("old hello = %+v, %v", old, err)
	}
}

func TestNormalizeHostSystemUUID(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"4C4C4544-0042-3510-8051-B4C04F4E3432\n", "4c4c4544-0042-3510-8051-b4c04f4e3432"},
		{"  ec2a1b2c-3d4e-5f60-7182-93a4b5c6d7e8\x00", "ec2a1b2c-3d4e-5f60-7182-93a4b5c6d7e8"},
		{"", ""},
		{" \n", ""},
		{"00000000-0000-0000-0000-000000000000", ""},
		{"FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF", ""},
		{"03000200-0400-0500-0006-000700080009", ""},
		{"Not Specified", ""},
		{"None", ""},
		{"N/A", ""},
		{"----", ""},
		{"4c4c4544;rm -rf", ""},
		{strings.Repeat("a1", 33), ""},
		{strings.Repeat("a1", 32), strings.Repeat("a1", 32)},
	} {
		if got := NormalizeHostSystemUUID(tc.raw); got != tc.want {
			t.Errorf("NormalizeHostSystemUUID(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
