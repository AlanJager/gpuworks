package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The production snapshot must not hide another process's committed metadata.
// A held WAL writer also must not make this hot HTTP read wait or leak writes.
func TestNodesHTTPSeesExternalCommitWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.db")
	st, err := store.Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	admin, ok := st.VerifyUser("admin", "gpuworks-test-password-2026")
	if !ok {
		t.Fatal("admin missing")
	}
	signer, err := auth.NewTokenSigner("external-commit-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: admin.ID, Username: admin.Username, Role: admin.Role})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer})
	get := func(path string) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+session)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	node, err := st.RegisterNode("external-node", store.HashToken("external-node"), "committed")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("name", "committed").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{Sys: proto.SysInfo{Hostname: "committed"}, GPUs: []proto.GPUInfo{{Index: 0, UUID: "external-card", Name: "committed-card", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	st.EnableFleetNodeSnapshot(nil)
	read := func(want, card string) {
		t.Helper()
		body := get("/api/v1/nodes")
		var page dto.List[dto.Node]
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].Name != want || len(page.Items[0].GPUs) != 1 || page.Items[0].GPUs[0].Name != card {
			t.Fatalf("GET /nodes = %s, want name %q", body, want)
		}
	}
	read("committed", "committed-card")
	writer, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := writer.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	tx := writer.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{"name": "external", "labels": `{"pool":"new"}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Model(&store.GPU{}).Where("node_id = ?", node.ID).Update("name", "external-card").Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); read("committed", "committed-card") }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GET /nodes blocked behind external writer")
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	read("external", "external-card")
	// Filtering and facets must agree with the committed row too.
	for _, path := range []string{"/api/v1/nodes?page=1&label=pool=new", "/api/v1/nodes/facets?label=pool=new"} {
		body := get(path)
		var count struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(body, &count); err != nil {
			t.Fatal(err)
		}
		if count.Total != 1 {
			t.Fatalf("GET %s = %s", path, body)
		}
	}
	// Facets itself must refresh, without a preceding list request warming it.
	if err := writer.Model(&store.Node{}).Where("id = ?", node.ID).Update("labels", `{"pool":"facet-only"}`).Error; err != nil {
		t.Fatal(err)
	}
	var facets struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(get("/api/v1/nodes/facets?label=pool=facet-only"), &facets); err != nil {
		t.Fatal(err)
	}
	if facets.Total != 1 {
		t.Fatalf("facets kept stale labels: %+v", facets)
	}
	cached, err := st.FleetNodes()
	if err != nil || len(cached) != 1 || cached[0].Labels != `{"pool":"facet-only"}` || cached[0].GPUs[0].Name != "external-card" {
		t.Fatalf("fresh HTTP view did not refresh shared fleet: %+v %v", cached, err)
	}
	// Losing the node read must report an error rather than serving a snapshot
	// that predates the external schema change.
	if err := writer.Exec("ALTER TABLE nodes RENAME TO unreadable_nodes").Error; err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+session)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unreadable nodes: %d %s", rec.Code, rec.Body)
	}
	if err := writer.Exec("ALTER TABLE unreadable_nodes RENAME TO nodes").Error; err != nil {
		t.Fatal(err)
	}
	if err := writer.Model(&store.Node{}).Where("id = ?", node.ID).Update("name", "recovered").Error; err != nil {
		t.Fatal(err)
	}
	recovered, err := st.FleetNodes()
	if err != nil || len(recovered) != 1 || recovered[0].Name != "recovered" {
		t.Fatalf("failed refresh kept an old snapshot: %+v %v", recovered, err)
	}

}
