package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The models page polls GET /models; where each model's files are is read
// in a fixed number of queries, not per model, storage or node.
func TestModelListQueryCountDoesNotGrowWithTheModels(t *testing.T) {
	count := func(models int) (int64, []dto.Model) {
		st, err := store.Open(":memory:", bus.New())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		nodes := make([]*store.Node, 3)
		for i := range nodes {
			if nodes[i], err = st.RegisterNode(fmt.Sprintf("n%d", i), store.HashToken(fmt.Sprint(i)), fmt.Sprintf("n%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		nfs, err := st.CreateStorage(&store.Storage{Name: "team-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
			Path: "/mnt/gpuworks/team-nfs", Export: "nfs.example.internal:/export/team"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range nodes {
			if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
				{ID: nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/team-nfs", Writable: node.ID != nodes[0].ID},
			}}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < models; i++ {
			m, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: fmt.Sprintf("org/m%d", i), Name: fmt.Sprintf("m%d", i)})
			if err != nil {
				t.Fatal(err)
			}
			for _, mf := range []store.ModelFile{
				{ModelID: m.ID, StorageID: nfs.ID, Path: fmt.Sprintf("org/m%d", i), State: proto.MFPresent, WriterNodeID: nodes[1].ID},
				{ModelID: m.ID, NodeID: nodes[i%3].ID, Path: fmt.Sprintf("/opt/gpuworks/models/m%d", i), State: proto.MFPresent, TransferSourceNodeID: nodes[(i+1)%3].ID},
			} {
				mf := mf
				if _, err := st.UpsertModelFile(&mf); err != nil {
					t.Fatal(err)
				}
			}
		}
		var queries atomic.Int64
		tally := func(*gorm.DB) { queries.Add(1) }
		if err := st.DB().Callback().Query().Before("gorm:query").Register("test:count_query", tally); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Callback().Row().Before("gorm:row").Register("test:count_row", tally); err != nil {
			t.Fatal(err)
		}
		h := &apiHandlers{d: Deps{Store: st}}
		rec := httptest.NewRecorder()
		h.listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
		}
		var out []dto.Model
		if err := unmarshalList(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return queries.Load(), out
	}
	few, small := count(3)
	many, large := count(40)
	if few != many {
		t.Fatalf("GET /models: %d queries for 3 models, %d for 40", few, many)
	}
	if len(small) != 3 || len(large) != 40 {
		t.Fatalf("models listed = %d, %d", len(small), len(large))
	}
	// The facts are the ones the single path gives.
	for _, m := range large {
		if len(m.Storages) != 1 || len(m.Nodes) != 1 {
			t.Fatalf("%s presence = %+v / %+v", m.Name, m.Storages, m.Nodes)
		}
		shared := m.Storages[0]
		if shared.ReadyNodes != 3 || !shared.Writable || shared.WriterNode == "" || m.Nodes[0].SourceNodeName == "" {
			t.Fatalf("%s presence = %+v / %+v", m.Name, shared, m.Nodes[0])
		}
	}
}
