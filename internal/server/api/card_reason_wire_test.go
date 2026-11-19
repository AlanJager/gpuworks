package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A card that takes no work must say why, on the wire, in both views.
// The verdict travels as one boolean so no client re-derives it; the
// reason travels beside it, because "unusable" alone sends an operator
// looking without telling them where. Both views sent the verdict and
// neither sent the reason, and nothing noticed because no test read the
// JSON these fields are declared in.
func TestACardThatTakesNoWorkSaysWhyOnTheWire(t *testing.T) {
	n := newNetworkAPI(t)
	if _, err := n.store.RegisterNode("reasons", store.HashToken("reasons"), "reasons"); err != nil {
		t.Fatal(err)
	}
	ok := true
	cards := []proto.GPUInfo{
		// Somebody else's process holds it: nothing ails the card.
		{Index: 0, UUID: "GPU-busy", Name: "A100", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0",
			MemoryTotalBytes: 80 << 30, Healthy: true,
			ForeignProcesses: []proto.CardProcess{{PID: 4242, Name: "vllm", MemoryBytes: 20 << 30}}},
		// Lent to a virtual machine: also not a fault.
		{Index: -1, Name: "A100", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", Ownership: accelerator.CardPassedThrough},
	}
	if err := n.store.UpdateNodeStatus("reasons", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards}); err != nil {
		t.Fatal(err)
	}
	if err := n.store.SetNodeState("reasons", store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}

	t.Run("node detail", func(t *testing.T) {
		raw := n.memberGet("/api/v1/nodes/reasons")
		var node dto.Node
		if err := json.Unmarshal(raw, &node); err != nil {
			t.Fatal(err)
		}
		byBDF := map[string]dto.GPU{}
		for _, g := range node.GPUs {
			byBDF[g.PCIeBDF] = g
		}
		busy := byBDF["0000:3f:00.0"]
		if busy.Schedulable || len(busy.ForeignProcesses) != 1 || busy.ForeignProcesses[0].Name != "vllm" {
			t.Fatalf("the card a foreign process holds = %+v, want the holder named", busy)
		}
		if byBDF["0000:40:00.0"].Ownership != accelerator.CardPassedThrough {
			t.Fatalf("the card lent to a VM = %+v, want its ownership", byBDF["0000:40:00.0"])
		}
		if !strings.Contains(string(raw), `"foreign_processes"`) {
			t.Fatal("the node detail JSON carries no foreign_processes key")
		}
	})

	t.Run("gpu list", func(t *testing.T) {
		raw := n.memberGet("/api/v1/gpus?page=1&page_size=10")
		var page dto.GPUPage
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		// Both cards are listed now: the one a foreign process holds and
		// the one lent to a virtual machine. Each says why it takes no
		// work, which is the point of listing them.
		if len(page.Items) != 2 {
			t.Fatalf("items = %d, want both of the node's cards", len(page.Items))
		}
		byBDF := map[string]dto.PhysicalGPU{}
		for _, g := range page.Items {
			byBDF[g.PCIeBDF] = g
		}
		if lent := byBDF["0000:40:00.0"]; lent.Ownership != accelerator.CardPassedThrough || lent.Schedulable {
			t.Fatalf("the card lent to a VM = %+v, want it listed with its ownership", lent)
		}
		item := byBDF["0000:3f:00.0"]
		if item.Schedulable || item.AllocationState != "unusable" {
			t.Fatalf("item = %+v, want it refused", item)
		}
		if len(item.ForeignProcesses) != 1 || item.ForeignProcesses[0].Name != "vllm" {
			t.Fatalf("item = %+v, want the process holding it named; \"unusable\" with no reason is the bug", item)
		}
		// The card is not faulty, and must not read as if it were.
		if item.HealthStatus != accelerator.HealthOK {
			t.Fatalf("health = %q, want OK: a busy card is not a broken one", item.HealthStatus)
		}
		// The wire itself, by key: a struct round trip would pass with a
		// misspelt tag on both ends.
		var wire struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		byWireBDF := map[string]map[string]any{}
		for _, item := range wire.Items {
			bdf, _ := item["pcie_bdf"].(string)
			byWireBDF[bdf] = item
		}
		assertJSONValues(t, byWireBDF["0000:40:00.0"], map[string]any{
			"ownership": accelerator.CardPassedThrough, "schedulable": false, "allocation_state": "unusable",
		})
		assertJSONKeysAbsent(t, byWireBDF["0000:40:00.0"], "foreign_processes", "healthy")
		assertJSONValues(t, byWireBDF["0000:3f:00.0"], map[string]any{
			"schedulable": false, "health_status": accelerator.HealthOK,
			"foreign_processes": []any{map[string]any{"pid": float64(4242), "name": "vllm", "memory_bytes": float64(20 << 30)}},
		})
		assertJSONKeysAbsent(t, byWireBDF["0000:3f:00.0"], "ownership", "healthy")
	})
}

// Why a node's runtime cannot give a container its cards, and which plugin
// it is missing, travel as a code and a name; the vendor tool's silence as
// its own reason and since when. The plugin's name is the server's to
// give — a client used to cut it out of an English display string.
func TestANodeSaysWhyItsRuntimeCannotUseItsCardsOnTheWire(t *testing.T) {
	n := newNetworkAPI(t)
	if _, err := n.store.RegisterNode("blocked", store.HashToken("blocked"), "blocked"); err != nil {
		t.Fatal(err)
	}
	failed := false
	if err := n.store.UpdateNodeStatus("blocked", proto.NodeStatus{
		GPUProbeOK: &failed, GPUProbeError: "nvidia-smi: no answer within 10s",
		Sys: proto.SysInfo{GPUVendor: "nvidia", RuntimeDriver: "docker", RuntimeGPUBlocked: accelerator.RuntimeGPUBlockedPluginMissing},
	}); err != nil {
		t.Fatal(err)
	}
	node := map[string]any{}
	if err := json.Unmarshal(n.memberGet("/api/v1/nodes/blocked"), &node); err != nil {
		t.Fatal(err)
	}
	assertJSONValues(t, node, map[string]any{
		"runtime_gpu_blocked": accelerator.RuntimeGPUBlockedPluginMissing,
		"runtime_gpu_plugin":  accelerator.PluginNVIDIAContainerToolkit,
		"scheduling":          "cordoned",
		"gpu_probe_error":     "nvidia-smi: no answer within 10s",
	})
	if at, _ := node["gpu_probe_failed_at"].(string); at == "" {
		t.Fatalf("gpu_probe_failed_at = %#v, want when the probes started failing", node["gpu_probe_failed_at"])
	}
	// Nothing is installed, so nothing is displayed as installed.
	assertJSONKeysAbsent(t, node, "runtime_plugin")
}

// A node on runveil because Docker could not use its cards says so on the
// wire: which runtime it passed over, why, and the plugin that runtime
// needs, named by the server. The node is not cordoned — runveil serves
// the cards — and none of the three keys is present on a node that passed
// nothing over.
func TestANodeSaysWhichRuntimeItPassedOverOnTheWire(t *testing.T) {
	n := newNetworkAPI(t)
	ok := true
	for _, uuid := range []string{"fallback", "plain"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.store.UpdateNodeStatus("fallback", proto.NodeStatus{
		GPUProbeOK: &ok,
		Sys: proto.SysInfo{GPUVendor: "nvidia", RuntimeDriver: "udocker",
			RuntimeFallbackFrom: "docker", RuntimeFallbackReason: accelerator.RuntimeGPUBlockedRuntimeUnconfigured},
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.store.UpdateNodeStatus("plain", proto.NodeStatus{
		GPUProbeOK: &ok, Sys: proto.SysInfo{GPUVendor: "nvidia", RuntimeDriver: "docker", RuntimePlugin: "NVIDIA Container Toolkit 1.17.3"},
	}); err != nil {
		t.Fatal(err)
	}
	node := map[string]any{}
	if err := json.Unmarshal(n.memberGet("/api/v1/nodes/fallback"), &node); err != nil {
		t.Fatal(err)
	}
	assertJSONValues(t, node, map[string]any{
		"runtime_driver":          "udocker",
		"runtime_fallback_from":   "docker",
		"runtime_fallback_reason": accelerator.RuntimeGPUBlockedRuntimeUnconfigured,
		"runtime_fallback_plugin": accelerator.PluginNVIDIAContainerToolkit,
		"scheduling":              "schedulable",
	})
	assertJSONKeysAbsent(t, node, "runtime_gpu_blocked", "runtime_gpu_plugin")

	plain := map[string]any{}
	if err := json.Unmarshal(n.memberGet("/api/v1/nodes/plain"), &plain); err != nil {
		t.Fatal(err)
	}
	assertJSONKeysAbsent(t, plain, "runtime_fallback_from", "runtime_fallback_reason", "runtime_fallback_plugin")
}

// A node whose Slurm will not run jobs on it says so on the wire: the code,
// Slurm's own words, the cordon with its reason, and, for a missing gres.conf,
// the line the server composed for the node's cards (one card, several cards).
// A node Slurm runs jobs on, and one that is not on Slurm, carry none of it.
func TestANodeSaysWhySlurmWillNotRunJobsOnTheWire(t *testing.T) {
	n := newNetworkAPI(t)
	ok := true
	card := func(i int) proto.GPUInfo {
		return proto.GPUInfo{Index: i, UUID: fmt.Sprintf("GPU-%d", i), Vendor: "nvidia", PCIeBDF: fmt.Sprintf("0000:%02x:00.0", 0x3f+i), MemoryTotalBytes: 24 << 30, Healthy: true}
	}
	for _, tc := range []struct {
		uuid  string
		cards []proto.GPUInfo
		sys   proto.SysInfo
	}{
		{"gres-two", []proto.GPUInfo{card(0), card(1)}, proto.SysInfo{SlurmNode: "gpu-1", SlurmUnavailable: proto.SlurmNodeGRESMissing, SlurmReason: "gres/gpu count reported lower than configured (0 < 2)"}},
		{"gres-one", []proto.GPUInfo{card(0)}, proto.SysInfo{SlurmNode: "gpu-2", SlurmUnavailable: proto.SlurmNodeGRESMissing, SlurmReason: "gres/gpu count reported lower than configured (0 < 1)"}},
		{"drained", []proto.GPUInfo{card(0), card(1)}, proto.SysInfo{SlurmNode: "gpu-3", SlurmUnavailable: proto.SlurmNodeDrained, SlurmReason: "maintenance"}},
		{"running", []proto.GPUInfo{card(0), card(1)}, proto.SysInfo{SlurmNode: "gpu-4"}},
		{"plain", []proto.GPUInfo{card(0)}, proto.SysInfo{}},
	} {
		if _, err := n.store.RegisterNode(tc.uuid, store.HashToken(tc.uuid), tc.uuid); err != nil {
			t.Fatal(err)
		}
		tc.sys.GPUVendor, tc.sys.RuntimeDriver = "nvidia", "docker"
		if err := n.store.UpdateNodeStatus(tc.uuid, proto.NodeStatus{GPUProbeOK: &ok, GPUs: tc.cards, Sys: tc.sys}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(uuid string) map[string]any {
		t.Helper()
		node := map[string]any{}
		if err := json.Unmarshal(n.memberGet("/api/v1/nodes/"+uuid), &node); err != nil {
			t.Fatal(err)
		}
		return node
	}
	assertJSONValues(t, get("gres-two"), map[string]any{
		"scheduling": "cordoned", "scheduling_by": "platform", "cordon_reason": "slurm_node_unavailable",
		"slurm_node": "gpu-1", "slurm_unavailable": "gres_missing",
		"slurm_reason": "gres/gpu count reported lower than configured (0 < 2)",
		"slurm_fixes": fixes(
			"gres_conf", []any{"NodeName=gpu-1 Name=gpu File=/dev/nvidia[0-1]"},
			"reconfigure", []any{"scontrol reconfigure"},
			"resume", []any{"scontrol update nodename=gpu-1 state=resume"},
			"restart_slurmd", []any{"sudo systemctl restart slurmd"}),
	})
	assertJSONValues(t, get("gres-one"), map[string]any{
		"cordon_reason": "slurm_node_unavailable",
		"slurm_fixes": fixes(
			"gres_conf", []any{"NodeName=gpu-2 Name=gpu File=/dev/nvidia0"},
			"reconfigure", []any{"scontrol reconfigure"},
			"resume", []any{"scontrol update nodename=gpu-2 state=resume"},
			"restart_slurmd", []any{"sudo systemctl restart slurmd"}),
	})
	drained := get("drained")
	assertJSONValues(t, drained, map[string]any{
		"cordon_reason": "slurm_node_unavailable", "slurm_node": "gpu-3", "slurm_unavailable": "drained", "slurm_reason": "maintenance",
		"slurm_fixes": fixes("resume_drained", []any{"scontrol update nodename=gpu-3 state=resume"}),
	})
	running := get("running")
	assertJSONValues(t, running, map[string]any{"scheduling": "schedulable", "slurm_node": "gpu-4"})
	assertJSONKeysAbsent(t, running, "slurm_unavailable", "slurm_reason", "slurm_fixes", "cordon_reason")
	assertJSONKeysAbsent(t, get("plain"), "slurm_node", "slurm_unavailable", "slurm_reason", "slurm_fixes")
}

// fixes is the wire form of slurm_fixes from code, commands pairs.
func fixes(pairs ...any) []any {
	var out []any
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, map[string]any{"code": pairs[i], "commands": pairs[i+1]})
	}
	return out
}
