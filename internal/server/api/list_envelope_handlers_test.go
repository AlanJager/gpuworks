package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TestNodesUnpagedAnswersListEnvelope: GET /nodes without paging answers the
// dto.List envelope - "items": [] with total 0 for an empty fleet - and each
// node's topology is the snake_case dto.NodeTopology read from the stored
// (PascalCase) nodes.topo_json.
func TestNodesUnpagedAnswersListEnvelope(t *testing.T) {
	n := newNetworkAPI(t)
	if got := strings.TrimSpace(string(n.memberGet("/api/v1/nodes"))); got != `{"items":[],"total":0}` {
		t.Fatalf("empty fleet = %s", got)
	}

	node, err := n.store.RegisterNode("topo-node", store.HashToken("topo"), "topo-node")
	if err != nil {
		t.Fatal(err)
	}
	stored := `{"GPUs":[{"Index":0,"CPUAffinity":"0-23","NUMANode":0}],"Matrix":[[{"Raw":"X","Class":7,"NVLinkBonds":0}]],"NVLinkCliques":[[0]],"NUMAGroups":{"0":[0]}}`
	if err := n.store.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("topo_json", stored).Error; err != nil {
		t.Fatal(err)
	}
	body := n.memberGet("/api/v1/nodes")
	var listed []map[string]json.RawMessage
	if err := unmarshalList(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("nodes = %s", body)
	}
	want := `{"gpus":[{"index":0,"cpu_affinity":"0-23","numa_node":0}],"matrix":[[{"raw":"X","class":7,"nvlink_bonds":0}]],"fast_cliques":[[0]],"nvlink_cliques":[[0]],"numa_groups":{"0":[0]}}`
	if got := string(listed[0]["topology"]); got != want {
		t.Fatalf("listed topology\n got %s\nwant %s", got, want)
	}
	// The single-node path answers the same topology.
	detail := n.memberGet("/api/v1/nodes/" + node.UUID)
	var one map[string]json.RawMessage
	if err := json.Unmarshal(detail, &one); err != nil {
		t.Fatal(err)
	}
	if got := string(one["topology"]); got != want {
		t.Fatalf("detail topology\n got %s\nwant %s", got, want)
	}
}

// TestEventTimelinesAnswerCursorPage: both event timelines answer the
// dto.CursorPage envelope - exactly {"items": [...]}, no total - and a page
// past the oldest event is "items": [], not null.
func TestEventTimelinesAnswerCursorPage(t *testing.T) {
	h := newEventsHarness(t, "api_events_cursor_page")
	for _, base := range []string{
		"/api/v1/instances/events-instance/events",
		fmt.Sprintf("/api/v1/deployments/%d/events", h.instance.DeploymentID),
	} {
		rec := h.get(t, base+"?before=1")
		if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || got != `{"items":[]}` {
			t.Fatalf("GET %s?before=1 = %d %s, want {\"items\":[]}", base, rec.Code, got)
		}
	}
	for _, state := range []string{proto.InstPulling, proto.InstRunning} {
		if err := h.store.UpdateInstanceStatus(proto.InstanceStatus{UUID: h.instance.UUID, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	for _, base := range []string{
		"/api/v1/instances/events-instance/events",
		fmt.Sprintf("/api/v1/deployments/%d/events", h.instance.DeploymentID),
	} {
		rec := h.get(t, base)
		var items []map[string]json.RawMessage
		if err := unmarshalCursorPage(rec.Body.Bytes(), &items); err != nil {
			t.Fatalf("GET %s: %v", base, err)
		}
		if len(items) == 0 {
			t.Fatalf("GET %s = %s, want the appended events", base, rec.Body.String())
		}
	}
}
