package deployment

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func rankScopedTemplate(t *testing.T, name string) (*store.Store, *Service, uint) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.Specs[0].WeightSources = `[{"provider":"modelscope","repo_id":"Acme/Deploy-TP3","revision":"r1","subfolder":"rank${NODE_RANK}"}]`
	fixture.Specs[0].WeightBytes = 5 << 30
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	return st, New(Deps{Store: st}), template.ID
}

func rankScopedSelection(t *testing.T, h *Service, templateID uint, nnodes int) (templateSelection, *store.Model) {
	t.Helper()
	req := dto.DeployRequest{TemplateID: templateID, TemplateSpecName: "fp8-balanced", NNodes: nnodes}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, templateID)
	if err != nil {
		t.Fatal(err)
	}
	return selection, model
}

// A rank-scoped spec resolves one artifact per rank at create (ADR 0076):
// rank 0's is the deployment's own model, every rank shares the repository
// and revision, each has its own subfolder, and the snapshot keeps the
// template's subfolder with the bindings.
func TestRankScopedTemplateResolvesOneArtifactPerRank(t *testing.T) {
	st, h, templateID := rankScopedTemplate(t, "rank_scoped_template_resolves")
	selection, model := rankScopedSelection(t, h, templateID, 3)
	if model.ArtifactSubfolder != "rank0" || model.SourceID != "Acme/Deploy-TP3" || model.Revision != "r1" || model.WeightBytes != 5<<30 {
		t.Fatalf("deployment model = %+v, want rank 0's artifact", model)
	}
	if len(selection.RankWeights) != 3 {
		t.Fatalf("rank weights = %+v", selection.RankWeights)
	}
	for rank, weight := range selection.RankWeights {
		artifact, err := st.GetModel(weight.StoreModelID)
		if err != nil {
			t.Fatal(err)
		}
		want := deploy.ExpandRankSubfolder("rank${NODE_RANK}", rank)
		if weight.Rank != rank || weight.Subfolder != want || artifact.ArtifactSubfolder != want || artifact.Source != "modelscope" || artifact.Revision != "r1" || artifact.SourceID != "Acme/Deploy-TP3" {
			t.Fatalf("rank %d binding %+v artifact %+v", rank, weight, artifact)
		}
	}
	if selection.RankWeights[0].StoreModelID != model.ID {
		t.Fatalf("rank 0 is not the deployment's own model: %+v", selection.RankWeights[0])
	}
	snapshot := snapshotTemplateSelection(selection, "vllm/vllm-openai:latest")
	if snapshot.WeightSubfolder != "rank${NODE_RANK}" || len(snapshot.RankWeights) != 3 {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	// Resolving again (an evaluation before the create) reuses the rows.
	again, model2 := rankScopedSelection(t, h, templateID, 3)
	if model2.ID != model.ID || again.RankWeights[2].StoreModelID != selection.RankWeights[2].StoreModelID {
		t.Fatalf("second resolution registered new rows: %+v vs %+v", again.RankWeights, selection.RankWeights)
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 3 {
		t.Fatalf("models = %d %v, want one per rank", len(models), err)
	}
}

// A single-node deployment of a rank-scoped spec reads rank 0's part only,
// and its snapshot binds that one rank.
func TestRankScopedTemplateOnOneNodeBindsRankZero(t *testing.T) {
	_, h, templateID := rankScopedTemplate(t, "rank_scoped_template_one_node")
	selection, model := rankScopedSelection(t, h, templateID, 1)
	if model.ArtifactSubfolder != "rank0" || len(selection.RankWeights) != 1 || selection.RankWeights[0].StoreModelID != model.ID {
		t.Fatalf("model %+v weights %+v", model, selection.RankWeights)
	}
}

// A named model must be rank 0's artifact: it is what rank 0 mounts. Another
// rank's subfolder of the same repository is refused with its own code.
func TestRankScopedTemplateRefusesAnotherRanksModel(t *testing.T) {
	st, h, templateID := rankScopedTemplate(t, "rank_scoped_template_named_model")
	rank1, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-TP3", Revision: "r1", ArtifactSubfolder: "rank1", Name: "Acme/Deploy-TP3"})
	if err != nil {
		t.Fatal(err)
	}
	req := dto.DeployRequest{TemplateID: templateID, TemplateSpecName: "fp8-balanced", NNodes: 2}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.resolveDeploymentModelFromTemplate(context.Background(), rank1.ID, &selection, templateID)
	if !containsProductCode(err, messages.ErrorRankWeightsModelNotRankZero) {
		t.Fatalf("rank 1's model as the deployment's = %v", err)
	}
	whole, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-TP3", Revision: "r1", Name: "whole"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), whole.ID, &selection, templateID); err == nil {
		t.Fatal("the whole repository was accepted for a rank-scoped spec")
	}
	if checkRankWeightsPD(selection, true) == nil || checkRankWeightsPD(selection, false) != nil {
		t.Fatal("PD refusal of rank-scoped weights")
	}
}

// What a create admits and downloads: each rank's artifact on its own rank's
// node only.
func TestRankScopedWeightTargetsPairRanksWithTheirNodes(t *testing.T) {
	_, h, templateID := rankScopedTemplate(t, "rank_scoped_weight_targets")
	selection, model := rankScopedSelection(t, h, templateID, 2)
	nodes := []*store.Node{{ID: 7, UUID: "lead"}, {ID: 8, UUID: "follow"}}
	targets, err := h.weightTargets(selection, model, append([]*store.Node{nodes[0]}, nodes...), nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %+v", targets)
	}
	for rank, target := range targets {
		if target.model.ID != selection.RankWeights[rank].StoreModelID || len(target.nodes) != 1 || target.nodes[0] != nodes[rank] {
			t.Fatalf("rank %d target = model %d nodes %+v", rank, target.model.ID, target.nodes)
		}
	}
	plain, err := h.weightTargets(templateSelection{}, model, nodes, nodes)
	if err != nil || len(plain) != 1 || len(plain[0].nodes) != 2 {
		t.Fatalf("unscoped targets = %+v %v", plain, err)
	}
	// The stored deployment reads per rank through planning.
	snapshot, err := catalogSelectionDocument(selection, "img")
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{ModelID: model.ID, CatalogSelectionSnapshot: snapshot}
	if ids, err := planning.RankModelIDs(dep, 1); err != nil || ids[0] != selection.RankWeights[1].StoreModelID {
		t.Fatalf("rank 1 models = %v %v", ids, err)
	}
}
