package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// go test ./internal/server/api -run '^TestStorageUsageObservedAt' -count=1
func TestStorageUsageObservedAtFollowsDiskReports(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("usage-observed", store.HashToken("token"), "storage")
	if err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		(&apiHandlers{d: Deps{Store: st}}).storageUsage(rec, httptest.NewRequest(http.MethodGet, "/api/v1/storages/usage", nil))
		var rows []map[string]any
		if err := unmarshalList(rec.Body.Bytes(), &rows); err != nil || rec.Code != http.StatusOK || len(rows) != 1 {
			t.Fatalf("usage = %s, status=%d, err=%v", rec.Body.String(), rec.Code, err)
		}
		return rows[0]
	}
	apply := func(report proto.NodeStatus) {
		t.Helper()
		if err := st.UpdateNodeStatus(node.UUID, report); err != nil {
			t.Fatal(err)
		}
	}
	setState := func(state string) {
		t.Helper()
		if err := st.SetNodeState(node.UUID, state); err != nil {
			t.Fatal(err)
		}
	}
	assertObserved := func(row map[string]any) {
		t.Helper()
		n, err := st.NodeByUUID(node.UUID)
		if err != nil || n.PathUsageObservedAt == nil {
			t.Fatalf("disk snapshot was not persisted: node=%+v, err=%v", n, err)
		}
		if want := n.PathUsageObservedAt.Format(time.RFC3339Nano); row["observed_at"] != want {
			t.Fatalf("observed_at=%v, want disk report time %s", row["observed_at"], want)
		}
	}
	if got := read(); got["observed_at"] != nil || got["capacity_known"] != false {
		t.Fatalf("never-reported node = %+v", got)
	}
	report := proto.NodeStatus{Sys: proto.SysInfo{Hostname: "storage"}, PathUsage: []proto.PathUsage{{Kind: "models", Path: "/models", TotalBytes: 1000, AvailableBytes: 400}}}
	apply(report)
	online := read()
	if err := st.TouchNodesAt(map[string]time.Time{node.UUID: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(got, online) {
		t.Fatalf("heartbeat changed capacity or its observation: %+v -> %+v", online, got)
	}
	setState(store.NodeStateOffline)
	offline := read()
	if !reflect.DeepEqual(offline, online) {
		t.Fatalf("offline node lost its last disk observation: %+v -> %+v", online, offline)
	}
	assertObserved(offline)
	setState(store.NodeStateOnline)
	apply(proto.NodeStatus{Sys: report.Sys})
	if got := read(); !reflect.DeepEqual(got, online) {
		t.Fatalf("reconnection without a disk report changed the snapshot: %+v -> %+v", online, got)
	}
	report.PathUsage[0].AvailableBytes = 200
	apply(report)
	updated := read()
	assertObserved(updated)
	if updated["available_bytes"] != float64(200) || updated["used_bytes"] != float64(800) {
		t.Fatalf("new disk report not reflected: %+v", updated)
	}
	// An upgraded database can have a historical snapshot with no observation time.
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("path_usage_observed_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	legacy := read()
	if legacy["observed_at"] != nil || legacy["capacity_known"] != true || legacy["available_bytes"] != float64(200) {
		t.Fatalf("legacy snapshot must keep capacity without inventing its time: %+v", legacy)
	}
}
