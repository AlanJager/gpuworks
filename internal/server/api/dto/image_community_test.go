package dto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCommunityTagMetadataWireContract(t *testing.T) {
	const wire = `{"tag":"cann9.0-sglang0.5.15.post1-arm64-diffusion-slot16","engine":"sglang","engine_version":"0.5.15.post1","framework":"cann","framework_version":"9.0","arch":"arm64"}`
	var tag CommunityTag
	if err := json.Unmarshal([]byte(wire), &tag); err != nil {
		t.Fatal(err)
	}
	want := CommunityTag{Tag: "cann9.0-sglang0.5.15.post1-arm64-diffusion-slot16", Engine: "sglang", EngineVersion: "0.5.15.post1", Framework: "cann", FrameworkVersion: "9.0", Arch: "arm64"}
	if !reflect.DeepEqual(tag, want) {
		t.Fatalf("decoded=%+v want=%+v", tag, want)
	}
	encoded, err := json.Marshal(tag)
	if err != nil || string(encoded) != wire {
		t.Fatalf("encoded=%s error=%v", encoded, err)
	}
	encoded, err = json.Marshal(CommunityTag{Tag: "latest", RollingTag: true})
	if err != nil || string(encoded) != `{"tag":"latest","rolling_tag":true}` {
		t.Fatalf("unknown metadata must be absent: %s error=%v", encoded, err)
	}
}
