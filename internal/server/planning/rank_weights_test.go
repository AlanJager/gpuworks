package planning

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func rankSelection() deploy.CatalogSelection {
	return deploy.CatalogSelection{WeightSubfolder: "rank${NODE_RANK}", RankWeights: []deploy.RankWeight{
		{Rank: 0, Subfolder: "rank0", StoreModelID: 10}, {Rank: 1, Subfolder: "rank1", StoreModelID: 11},
	}}
}

// The artifact a rank reads is the deployment's model under the rank's
// identity; weights the same on every rank have none.
func TestRankWeightModelIsTheRanksArtifact(t *testing.T) {
	model := &store.Model{ID: 10, SourceID: "example/ranked", ArtifactSubfolder: "rank0", WeightBytes: 7, Arch: "Ranked"}
	artifact, ranks := rankWeightModel(rankSelection(), model, 1)
	if artifact == nil || ranks != 2 || artifact.ID != 11 || artifact.ArtifactSubfolder != "rank1" || artifact.WeightBytes != 7 || artifact.Arch != "Ranked" {
		t.Fatalf("rank 1 artifact = %+v ranks=%d", artifact, ranks)
	}
	if model.ID != 10 || model.ArtifactSubfolder != "rank0" {
		t.Fatalf("the deployment's model was changed: %+v", model)
	}
	if artifact, ranks := rankWeightModel(deploy.CatalogSelection{}, model, 1); artifact != nil || ranks != 0 {
		t.Fatalf("unscoped selection = %+v %d", artifact, ranks)
	}
	if artifact, _ := rankWeightModel(rankSelection(), model, 5); artifact != nil {
		t.Fatalf("an unbound rank got %+v", artifact)
	}
	if (Input{Model: model}).storageModel() != model || (Input{Model: model, WeightModel: artifact}).storageModel() != artifact {
		t.Fatal("storageModel does not prefer the rank's artifact")
	}
}

// RankModelIDs and NodeModelIDs read only the deployment row: rank r's
// primary is its own artifact, and a node's models are its rank's.
func TestRankModelIDsFollowTheRank(t *testing.T) {
	snapshot, err := rankSelection().Encode()
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := json.Marshal([]uint{3, 4})
	dep := &store.Deployment{ID: 1, ModelID: 10, NNodes: 2, NodeIDs: string(ids), CatalogSelectionSnapshot: string(snapshot)}
	for rank, want := range []uint{10, 11} {
		got, err := RankModelIDs(dep, rank)
		if err != nil || len(got) != 1 || got[0] != want {
			t.Fatalf("rank %d models = %v %v, want [%d]", rank, got, err, want)
		}
	}
	all, err := DeploymentModelIDs(dep)
	if err != nil || len(all) != 2 || all[0] != 10 || all[1] != 11 {
		t.Fatalf("deployment models = %v %v", all, err)
	}
	nodeModels, err := NodeModelIDs(dep)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeModels(&store.Node{ID: 4}); len(got) != 1 || got[0] != 11 {
		t.Fatalf("rank 1's node reads %v", got)
	}
	if got := nodeModels(&store.Node{ID: 9}); len(got) != 2 {
		t.Fatalf("a node the deployment does not place reads %v, want every rank's", got)
	}
	// A listed replica places its nodes by UUID, ahead of the node list.
	dep.Placement = `{"tp":1,"units":[{"node_uuid":"lead","followers":[{"node_uuid":"follow"}]}]}`
	if nodeModels, err = NodeModelIDs(dep); err != nil {
		t.Fatal(err)
	}
	if got := nodeModels(&store.Node{ID: 3, UUID: "follow"}); len(got) != 1 || got[0] != 11 {
		t.Fatalf("listed follower reads %v", got)
	}
	// A broken binding is an error, not a silent fallback to rank 0's model.
	broken, _ := deploy.CatalogSelection{RankWeights: []deploy.RankWeight{{Rank: 0, StoreModelID: 10}, {Rank: 1}}}.Encode()
	if _, err := DeploymentModelIDs(&store.Deployment{ModelID: 10, CatalogSelectionSnapshot: string(broken)}); err == nil {
		t.Fatal("a rank without a model binding was accepted")
	}
}

// Disk admission on a rank's node counts the rank's own artifact: a node
// that already holds rank 1's subfolder needs no disk for it, although it
// has none of rank 0's. The estimate holds every rank's share.
func TestRankScopedAdmissionCountsTheRanksArtifact(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("rank-one", store.HashToken("rank-one"), "rank-one")
	if err != nil {
		t.Fatal(err)
	}
	node.PathUsage = `[{"kind":"models","available_bytes":1048576}]`
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0"}}
	var models []*store.Model
	for _, subfolder := range []string{"rank0", "rank1"} {
		model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "example/ranked", ArtifactSubfolder: subfolder, Name: "example/ranked", WeightBytes: 4 << 30})
		if err != nil {
			t.Fatal(err)
		}
		models = append(models, model)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: models[1].ID, StorageID: st.DefaultLocalStorageID(), NodeID: node.ID,
		Path: "/opt/gpuworks/models/rank1", State: proto.MFPresent, SizeBytes: 4 << 30, Downloaded: 4 << 30}); err != nil {
		t.Fatal(err)
	}
	selection := deploy.CatalogSelection{WeightSubfolder: "rank${NODE_RANK}", RankWeights: []deploy.RankWeight{
		{Rank: 0, Subfolder: "rank0", StoreModelID: models[0].ID}, {Rank: 1, Subfolder: "rank1", StoreModelID: models[1].ID},
	}}
	weightModel, ranks := rankWeightModel(selection, models[0], 1)
	in := Input{Model: models[0], WeightModel: weightModel, WeightRanks: ranks, Node: node, Engine: "vllm", Quick: deploy.LaunchConfig{GPUMemUtil: 0.9}}
	out := in.Evaluate(st)
	if !out.Result.OK {
		t.Fatalf("rank 1's node holding its own artifact was refused: %+v", out.Result)
	}
	if out.Breakdown.WeightBytes != 8<<30 {
		t.Fatalf("estimated weights = %d, want both ranks' share (%d)", out.Breakdown.WeightBytes, int64(8<<30))
	}
	// The same node judged by rank 0's artifact has to download it and has no
	// room: the counterfactual the rank binding prevents.
	in.WeightModel, in.WeightRanks = nil, 0
	if out := in.Evaluate(st); out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientDisk {
		t.Fatalf("rank 0's artifact on rank 1's node = %+v, want insufficient disk", out.Result)
	}
}
