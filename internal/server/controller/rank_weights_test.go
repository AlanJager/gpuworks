package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Rank-scoped weights (ADR 0076): each rank of a multi-node replica downloads
// and mounts only its own artifact. Rank 1's node gets a download task for
// rank 1's subfolder and none for rank 0's; the replica waits for it, and
// once it is there each rank records and mounts its own files.
func TestRankScopedWeightsEachRankReadsItsOwnArtifact(t *testing.T) {
	st, err := store.Open("file:rank_scoped_weights_own_artifact?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var models []*store.Model
	for rank := range 2 {
		model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "example/ranked", Revision: "main", ArtifactSubfolder: fmt.Sprintf("rank%d", rank), Name: "example/ranked", WeightBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		models = append(models, model)
	}
	var nodes []*store.Node
	online := map[string]bool{}
	for rank := range 2 {
		uuid := fmt.Sprintf("rank-weights-%d", rank)
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		node.NetworkInventory = managementInventoryJSON(t, fmt.Sprintf("10.7.0.%d", rank+1))
		if err := st.DB().Save(node).Error; err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: uuid + "-gpu", Name: "GPU", Vendor: "nvidia", MemoryTotal: 24 << 30}).Error; err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
		online[uuid] = true
	}
	// Rank 0's artifact is on rank 0's node; rank 1's node has nothing yet.
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: models[0].ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: "/models/rank0"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := deploy.CatalogSelection{
		ModelID: "example/ranked", SpecName: "tp2", Engine: "vllm", WeightProvider: "modelscope", WeightRepoID: "example/ranked",
		WeightRevision: "main", WeightSubfolder: "rank${NODE_RANK}", WeightBytes: 1 << 20,
		RankWeights: []deploy.RankWeight{{Rank: 0, Subfolder: "rank0", StoreModelID: models[0].ID}, {Rank: 1, Subfolder: "rank1", StoreModelID: models[1].ID}},
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := json.Marshal([]uint{nodes[0].ID, nodes[1].ID})
	dep, err := createTestDeployment(t, st, &store.Deployment{
		ModelID: models[0].ID, NodeID: nodes[0].ID, NodeIDs: string(ids), ServedName: "ranked", Engine: "vllm", State: "active",
		NNodes: 2, MasterAddr: managementIP(*nodes[0]), MasterPort: 29500, Placement: `{"tp":2}`, CatalogSelectionSnapshot: string(snapshot),
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: online, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if instances, err := st.InstancesForDeployment(dep.ID); err != nil || len(instances) != 0 {
		t.Fatalf("the replica started before rank 1's weights arrived: %+v %v", instances, err)
	}
	if _, err := st.ModelFileAt(store.FileRef{ModelID: models[0].ID, NodeID: nodes[1].ID}); err == nil {
		t.Fatal("rank 1's node was given rank 0's artifact to download")
	}
	task, err := st.ModelFileAt(store.FileRef{ModelID: models[1].ID, NodeID: nodes[1].ID})
	if err != nil {
		t.Fatalf("rank 1's node has no download task for its own artifact: %v", err)
	}
	if _, err := st.ModelFileAt(store.FileRef{ModelID: models[1].ID, NodeID: nodes[0].ID}); err == nil {
		t.Fatal("rank 0's node was given rank 1's artifact to download")
	}

	task.State, task.Path = proto.MFPresent, "/models/rank1"
	if _, err := st.UpsertModelFile(task); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 2 {
		t.Fatalf("instances once every rank's weights are there = %+v %v", instances, err)
	}
	instanceIDs := make([]uint, 0, len(instances))
	for _, inst := range instances {
		instanceIDs = append(instanceIDs, inst.ID)
	}
	reads, err := st.InstanceModelFiles(instanceIDs)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		want := models[inst.NodeRank]
		if inst.NodeID != nodes[inst.NodeRank].ID {
			t.Fatalf("rank %d runs on node %d, want %d", inst.NodeRank, inst.NodeID, nodes[inst.NodeRank].ID)
		}
		got := reads[inst.ID]
		if len(got) != 1 || got[0].ModelID != want.ID {
			t.Fatalf("rank %d records reads %+v, want only model %d", inst.NodeRank, got, want.ID)
		}
		var desired proto.InstanceDesiredState
		if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &desired); err != nil {
			t.Fatal(err)
		}
		if wantPath := fmt.Sprintf("/models/rank%d", inst.NodeRank); desired.ModelPath != wantPath {
			t.Fatalf("rank %d mounts %q, want %q", inst.NodeRank, desired.ModelPath, wantPath)
		}
	}

	// Every rank's artifact is the deployment's dependency (download
	// ownership, eviction guards); each node's is only its rank's.
	all, err := planning.DeploymentModelIDs(dep)
	if err != nil || len(all) != 2 || all[0] != models[0].ID || all[1] != models[1].ID {
		t.Fatalf("deployment models = %v %v", all, err)
	}
	nodeModels, err := planning.NodeModelIDs(dep)
	if err != nil {
		t.Fatal(err)
	}
	for rank, node := range nodes {
		if got := nodeModels(node); len(got) != 1 || got[0] != models[rank].ID {
			t.Fatalf("node of rank %d reads %v, want [%d]", rank, got, models[rank].ID)
		}
	}
}
