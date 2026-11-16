package deploy

import (
	"slices"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/kvbackend"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// testExternalKVBackend is a backend the product does not ship. It exercises
// every generic rule an external row is held to, so the seam is tested
// before a real third-party backend lands on it.
const testExternalKVBackend = "testext"

// testExternalKVBackendKeys are the native dictionary keys the test row
// replaces, the ones the deploy form tombstones when its card is chosen.
var testExternalKVBackendKeys = []string{
	"kv_offloading_size", "kv_disk_offload", "shared_kv_dir",
	"kv_offload_read_threads", "kv_offload_write_threads", "kv_disk_limit_gib",
}

func testExternalKVBackendRow() kvbackend.Backend {
	return kvbackend.Backend{
		ID: testExternalKVBackend, Engine: EngineVLLM, Label: "Test External", External: true,
		Vendors: []string{accelerator.VendorNVIDIA},
		// All of vLLM's own offload, including the block size, which has no
		// dictionary row (nativeKVKeys).
		Replaces: append(slices.Clone(testExternalKVBackendKeys), "kv_offload_block_size"),
		PDRoles:  []string{pdmode.RolePrefill}, Topology: kvbackend.Topology{MaxNNodes: 1},
	}
}

// testExternalVLLMOffload renders the test row with a connector name package
// engines does not know, as a real third-party connector would be: a non-PD
// launch must still not read as PD. It reads its launch context the way a
// real backend does: on a PD prefill its own role follows the PD role and it
// shapes the MultiConnector around it, and under data parallelism it sets
// environment for the engine process.
func testExternalVLLMOffload(p Params) *kvContribution {
	part := &kvContribution{object: map[string]any{
		"kv_connector": "TestExternalConnector", "kv_role": "kv_both",
		"kv_connector_extra_config": map[string]any{"spec_name": "TestExternalSpec"},
	}}
	if p.PD != nil && p.PD.Role == pdmode.RolePrefill {
		part.object["kv_role"] = "kv_producer"
		part.wrapper = map[string]any{"kv_connector_module_path": testExternalWrapperModule}
	}
	if p.Quick.DP > 1 {
		part.env = map[string]string{testExternalEnv: strconv.Itoa(p.Quick.DP)}
	}
	return part
}

// testExternalWrapperModule and testExternalEnv are what the test row's
// renderer adds from its launch context.
const (
	testExternalWrapperModule = "testext.multi_connector"
	testExternalEnv           = "TESTEXT_DP_SIZE"
)

// testExternalKVTransfer is the --kv-transfer-config value the test row
// renders without PD.
const testExternalKVTransfer = `{"kv_connector":"TestExternalConnector","kv_connector_extra_config":{"spec_name":"TestExternalSpec"},"kv_role":"kv_both"}`

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
	kvBackends = kvBackendSet{registry: registry, vllmOffload: map[string]vllmOffloadBackend{testExternalKVBackend: {render: testExternalVLLMOffload}}}
	t.Cleanup(func() { kvBackends = previous })
}
