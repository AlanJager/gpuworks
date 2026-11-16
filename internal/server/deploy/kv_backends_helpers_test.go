package deploy

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/kvbackend"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// testExternalKVBackend is a backend the product does not ship. It exercises
// every generic rule an external row is held to, so the seam is tested
// before a real third-party backend lands on it.
const testExternalKVBackend = "testext"

// testExternalKVBackendKeys are the native keys the test row replaces: all of
// vLLM's own offload.
var testExternalKVBackendKeys = []string{
	"kv_offloading_size", "kv_disk_offload", "shared_kv_dir",
	"kv_offload_read_threads", "kv_offload_write_threads", "kv_disk_limit_gib",
}

func testExternalKVBackendRow() kvbackend.Backend {
	return kvbackend.Backend{
		ID: testExternalKVBackend, Engine: EngineVLLM, Label: "Test External", External: true,
		Vendors: []string{accelerator.VendorNVIDIA}, Replaces: testExternalKVBackendKeys,
		PDRoles: []string{pdmode.RolePrefill}, Topology: kvbackend.Topology{MaxNNodes: 1},
	}
}

// testExternalVLLMOffload renders the test row as an OffloadingConnector
// object with its own spec, so connector-based PD classification reads it as
// offload and the recognizer can tell it from the native disk object.
func testExternalVLLMOffload(LaunchConfig) *kvContribution {
	return &kvContribution{object: map[string]any{
		"kv_connector": "OffloadingConnector", "kv_role": "kv_both",
		"kv_connector_extra_config": map[string]any{"spec_name": "TestExternalSpec"},
	}}
}

// testExternalKVTransfer is the --kv-transfer-config value the test row
// renders without PD.
const testExternalKVTransfer = `{"kv_connector":"OffloadingConnector","kv_connector_extra_config":{"spec_name":"TestExternalSpec"},"kv_role":"kv_both"}`

// withKVBackends swaps the package's catalog for the shipped rows plus the
// test row for the rest of the test. Safe because this package's tests do not
// run in parallel (t.Parallel is not used here); a parallel test would see the
// swapped catalog.
func withKVBackends(t *testing.T) {
	t.Helper()
	native, ok := kvbackend.Builtin().Resolve(EngineVLLM, "")
	if !ok {
		t.Fatal("no native vLLM row")
	}
	registry, err := kvbackend.New(native, testExternalKVBackendRow())
	if err != nil {
		t.Fatal(err)
	}
	previous := kvBackends
	kvBackends = kvBackendSet{registry: registry, vllmOffload: map[string]vllmOffloadRenderer{testExternalKVBackend: testExternalVLLMOffload}}
	t.Cleanup(func() { kvBackends = previous })
}
