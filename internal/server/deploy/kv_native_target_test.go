package deploy

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestNativeKVOffloadRejectsAscendBeforeLaunching(t *testing.T) {
	for _, q := range []LaunchConfig{{KVOffloadingSize: 2}, {KVDiskOffload: true}, {KVOffloadingSize: 2, KVDiskOffload: true}} {
		wantCode(t, validateKVBackendTarget(EngineVLLM, requestedTopology{nodes: 1}, Params{Vendor: "ascend", Quick: q}), messages.ErrorKVOffloadBackendUnsupportedOnTarget)
		for _, vendor := range []string{"nvidia", ""} {
			if err := validateKVBackendTarget(EngineVLLM, requestedTopology{nodes: 1}, Params{Vendor: vendor, Quick: q}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := validateKVBackendTarget(EngineVLLM, requestedTopology{nodes: 1}, Params{Vendor: "ascend"}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeKVOffloadRecognizesEffectiveArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"cpu", []string{"--kv-offloading-size", "2"}, true},
		{"underscore", []string{"--kv_offloading_size=2"}, true},
		{"disabled", []string{"--kv-offloading-size", "0"}, false},
		{"last disabled", []string{"--kv-offloading-size=2", "--kv-offloading-size=0"}, false},
		{"disk", []string{"--kv-transfer-config", `{"kv_connector":"OffloadingConnector"}`}, true},
		{"pd only", []string{"--kv-transfer-config", `{"kv_connector":"NixlConnector"}`}, false},
		{"combined", []string{"--kv-transfer-config", `{"kv_connector":"MultiConnector","kv_connector_extra_config":{"connectors":[{"kv_connector":"NixlConnector"},{"kv_connector":"OffloadingConnector"}]}}`}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeKVOffloadInArgs(tc.args); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestNativeKVOffloadResolveChecksBothValidationModes(t *testing.T) {
	for _, mode := range []TargetValidationMode{ValidateTargetCapabilities, ValidateAssignedTarget} {
		_, err := resolveKVTest(kvTestParams(LaunchConfig{KVOffloadingSize: 2}, "ascend"), mode)
		wantCode(t, err, messages.ErrorKVOffloadBackendUnsupportedOnTarget)
		p := kvTestParams(LaunchConfig{KVOffloadingSize: 2}, "ascend")
		p.ExtraArgs = []string{"--kv-offloading-size=0"}
		if _, err := resolveKVTest(p, mode); err != nil {
			t.Fatalf("disabled final offload must be allowed: %v", err)
		}
	}
}
