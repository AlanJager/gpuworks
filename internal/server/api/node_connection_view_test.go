package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A fresh server has no old authenticated sessions even when SQLite still
// says online. Test the public views together; a node badge alone can hide
// contradictory filters, fleet counts and available GPU capacity.
func TestNodeHTTPViewsUseAuthenticatedConnectionsWithoutChangingStoredFacts(t *testing.T) {
	live := map[string]bool{"live": true}
	r := newTokenRig(t, func(d *Deps) { d.Online = func(uuid string) bool { return live[uuid] } })
	t.Cleanup(func() { _ = r.st.Close() })
	for _, item := range []struct{ uuid, state string }{
		{"live", store.NodeStateOnline},
		{"reconnecting", store.NodeStateOnline},
		{"offline", store.NodeStateOffline},
	} {
		if _, err := r.st.RegisterNode(item.uuid, store.HashToken(item.uuid), item.uuid); err != nil {
			t.Fatal(err)
		}
		if err := r.st.UpdateNodeStatus(item.uuid, proto.NodeStatus{Sys: proto.SysInfo{Hostname: item.uuid, GPUVendor: "nvidia"}, GPUs: []proto.GPUInfo{{Index: 0, UUID: item.uuid + "-card", Name: "H100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}}}); err != nil {
			t.Fatal(err)
		}
		if err := r.st.SetNodeState(item.uuid, item.state); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.st.TouchNodesAt(map[string]time.Time{"reconnecting": time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := r.st.NodeWithGPUs("reconnecting")
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string, result any) {
		t.Helper()
		w := r.do(r.session, http.MethodGet, path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
			t.Fatal(err)
		}
	}
	wantStates := map[string]string{"live": "online", "reconnecting": "connecting", "offline": "offline"}
	for uuid, state := range wantStates {
		var node dto.Node
		read("/api/v1/nodes/"+uuid, &node)
		if node.State != state {
			t.Errorf("detail %s state=%s, want %s", uuid, node.State, state)
		}
		var page dto.NodePage
		read("/api/v1/nodes?page=1&page_size=10&state="+state, &page)
		if page.Total != 1 || len(page.Items) != 1 || page.Items[0].UUID != uuid || page.Items[0].State != state {
			t.Errorf("%s filtered total=%d items=%d, want exactly %s", state, page.Total, len(page.Items), uuid)
		}
	}
	var summary dto.DashboardSummary
	read("/api/v1/dashboard/summary", &summary)
	if summary.Nodes.Total != 3 || summary.Nodes.Online != 1 || summary.Nodes.Connecting != 1 || summary.Nodes.Offline != 1 {
		t.Errorf("dashboard node counts=%+v", summary.Nodes)
	}
	var inventory dto.GPUListResponse
	read("/api/v1/gpus", &inventory)
	for _, card := range inventory.Items {
		if card.NodeState != wantStates[card.NodeUUID] {
			t.Errorf("GPU %s node state=%s, want %s", card.UUID, card.NodeState, wantStates[card.NodeUUID])
		}
		if card.NodeUUID != "live" && (card.AvailableBytes != 0 || card.AllocationState != "unknown") {
			t.Errorf("unreachable GPU claims capacity: %+v", card)
		}
	}
	if len(inventory.Items) != 3 {
		t.Errorf("GPU items=%d, want 3", len(inventory.Items))
	}
	image, err := r.st.UpsertImage(&store.Image{ImageRef: "repo/connection-view:test", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var series dto.ImageSeries
	read("/api/v1/images/"+strconv.FormatUint(uint64(image.SeriesID), 10), &series)
	if len(series.Versions) != 1 || len(series.Versions[0].Nodes) != 3 {
		t.Fatalf("image node view=%+v", series)
	}
	for _, node := range series.Versions[0].Nodes {
		if node.NodeState != wantStates[node.NodeUUID] {
			t.Errorf("image node %s state=%s", node.NodeUUID, node.NodeState)
		}
	}
	storageID := r.st.DefaultLocalStorageID()
	for uuid := range wantStates {
		node, err := r.st.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: storageID, State: proto.StorageStateReady, Path: "/models", Writable: true}}}); err != nil {
			t.Fatal(err)
		}
	}
	var storageView dto.Storage
	read("/api/v1/storages/"+strconv.FormatUint(uint64(storageID), 10)+"?all_nodes=1", &storageView)
	if storageView.Nodes.Total != 3 || storageView.Nodes.Ready != 1 {
		t.Errorf("storage node totals=%+v", storageView.Nodes)
	}
	for _, node := range storageView.Nodes.All {
		want := "offline"
		if node.NodeUUID == "live" {
			want = proto.StorageStateReady
		}
		if node.State != want {
			t.Errorf("storage node %s state=%s want=%s", node.NodeUUID, node.State, want)
		}
	}
	after, err := r.st.NodeWithGPUs("reconnecting")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("presentation changed stored node facts: before=%+v after=%+v err=%v", before, after, err)
	}
	// A real session is authoritative even before its queued database status
	// write lands. Once the existing stale policy marks the stored projection
	// offline, the presentation must stop saying it is reconnecting.
	live["reconnecting"] = true
	var node dto.Node
	read("/api/v1/nodes/reconnecting", &node)
	if node.State != "online" {
		t.Errorf("authenticated connection state=%s, want online", node.State)
	}
	delete(live, "reconnecting")
	if err := r.st.MarkStaleOffline(t.Context(), time.Minute); err != nil {
		t.Fatal(err)
	}
	read("/api/v1/nodes/reconnecting", &node)
	if node.State != "offline" || node.LastSeenAt == nil {
		t.Errorf("expired connection=%+v", node)
	}
}
