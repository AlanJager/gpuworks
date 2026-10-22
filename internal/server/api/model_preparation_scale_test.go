package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TestModelPreparationFleetListLoad measures page reads during real store/bus
// progress ingestion, with 1000 nodes and 3000 preparing deployments.
// GPUWORKS_TEST_PERF=1 go test -count=1 -v ./internal/server/api -run '^TestModelPreparationFleetListLoad$'
func TestModelPreparationFleetListLoad(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("explicit fleet load test")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	nodes := make([]store.Node, 1000)
	for i := range nodes {
		nodes[i] = store.Node{UUID: fmt.Sprintf("prep-%d", i), Name: fmt.Sprintf("prep-%d", i), State: store.NodeStateOnline}
	}
	if err := st.DB().CreateInBatches(&nodes, 100).Error; err != nil {
		t.Fatal(err)
	}
	models := make([]store.Model, 3)
	for i := range models {
		models[i] = store.Model{Source: "local", SourceID: fmt.Sprint(i), Name: fmt.Sprintf("model-%d", i)}
	}
	if err := st.DB().Create(&models).Error; err != nil {
		t.Fatal(err)
	}
	storage, err := st.CreateStorage(&store.Storage{Name: "cache", Type: proto.StorageNFSCache, Enabled: true, Path: "/nfs", Export: "fixture:/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit fleet membership catches accidental per-node rescans of all
	// storage targets; AllNodes alone would hide that quadratic path.
	targets := make([]store.StorageNode, len(nodes))
	for i, node := range nodes {
		targets[i] = store.StorageNode{StorageID: storage.ID, NodeID: node.ID}
	}
	if err := st.DB().CreateInBatches(&targets, 100).Error; err != nil {
		t.Fatal(err)
	}
	states := make([]store.StorageNodeState, len(nodes))
	var files []store.ModelFile
	var deps []store.Deployment
	for i, node := range nodes {
		states[i] = store.StorageNodeState{NodeID: node.ID, StorageID: storage.ID, State: proto.StorageStateReady, Path: "/nfs", CachePath: "/cache"}
		for _, model := range models {
			files = append(files, store.ModelFile{NodeID: node.ID, ModelID: model.ID, StorageID: storage.ID, State: proto.MFDownloading, Acquisition: proto.ModelAcquireCopy, TransferID: fmt.Sprintf("%d-%d", node.ID, model.ID), SizeBytes: 1 << 30})
			deps = append(deps, store.Deployment{NodeID: node.ID, ModelID: model.ID, Name: fmt.Sprintf("prepare-%d-%d", node.ID, model.ID), State: store.DeployActive, SaveToStorageID: storage.ID, Replicas: 1})
		}
	}
	for _, model := range models {
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storage.ID, State: proto.MFPresent, Path: fmt.Sprint(model.ID), SizeBytes: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	for _, rows := range []any{&states, &files, &deps} {
		if err := st.DB().CreateInBatches(rows, 100).Error; err != nil {
			t.Fatal(err)
		}
	}
	prep := newTestPreparation()
	var writes, frames atomic.Int64
	if err := st.DB().Callback().Update().Before("gorm:update").Register("prep_scale_writes", func(*gorm.DB) { writes.Add(1) }); err != nil {
		t.Fatal(err)
	}
	defer st.DB().Callback().Update().Remove("prep_scale_writes")
	stop, ended := make(chan struct{}), make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for round := int64(1); ; round++ {
			for _, file := range files {
				select {
				case <-stop:
					ended <- nil
					return
				default:
				}
				frame := proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), TransferID: file.TransferID, State: proto.MFDownloading, BytesTotal: file.SizeBytes, BytesDone: round * 4096, Preparation: &proto.ModelPreparation{Attempt: "load", Phase: proto.ModelPhaseCopying, TotalKnown: true}}
				prep.record(file.NodeID, frame)
				err := st.UpdateModelFileStatus(file.NodeID, frame)
				if err != nil {
					ended <- err
					return
				}
				frames.Add(1)
			}
			select {
			case <-stop:
				ended <- nil
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-ended; err != nil {
			t.Error(err)
		}
	}()
	h := &apiHandlers{d: Deps{Store: st, Preparation: prep}}
	started := time.Now()
	durations := make([]time.Duration, 0, 20)
	for i := 0; i < 20 || time.Since(started) < 3*time.Second; i++ {
		response := httptest.NewRecorder()
		start := time.Now()
		h.listDeployments(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/deployments?page=%d&page_size=20", i%20+1), nil))
		durations = append(durations, time.Since(start))
		if response.Code != http.StatusOK {
			t.Fatalf("page status=%d: %s", response.Code, response.Body.String())
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if writes.Load() != 0 {
		t.Fatalf("progress/list caused %d writes", writes.Load())
	}
	if frames.Load() < int64(len(files)) {
		t.Fatal("did not ingest a full fleet progress cycle")
	}
	t.Logf("1000 nodes / 3000 preparing deployments, %d real list pages over %s while ingesting %d progress frames: p50=%s p99=%s, writes=%d", len(durations), time.Since(started), frames.Load(), durations[len(durations)/2], durations[(len(durations)-1)*99/100], writes.Load())
}
