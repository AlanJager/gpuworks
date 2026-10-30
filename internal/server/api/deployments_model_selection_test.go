package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func templateFailedRevisionFixture(t *testing.T, ready bool) (*templateStorageFixture, *store.Model) {
	t.Helper()
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, ready, false)
	catalog := deploymentFlatFixture()
	catalog.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"Acme/Deploy-FP8","revision":"template-rev"},{"provider":"modelscope","repo_id":"Acme/Deploy-FP8","revision":"template-rev"}]`
	if err := f.h.d.Store.ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	bad, err := f.h.d.Store.UpsertModel(&store.Model{Source: "modelscope", SourceID: f.shared.SourceID, Revision: "template-rev"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.d.Store.UpsertModelFile(&store.ModelFile{ModelID: bad.ID, StorageID: f.nfs.ID, Path: "Acme/failed-revision", State: proto.MFFailed, Message: "revision returned HTTP 404"}); err != nil {
		t.Fatal(err)
	}
	return f, bad
}

func TestTemplateExplicitModelIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ready    bool
		provider string
	}{
		{"ready with provider", true, "modelscope"},
		{"ready without provider", true, ""},
		{"unready with provider", false, "modelscope"},
		{"unready without provider", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := templateFailedRevisionFixture(t, tc.ready)
			req := f.request()
			req.ModelID = f.shared.ID
			if tc.provider != "" {
				req.ModelSource = &dto.ModelSource{Provider: tc.provider}
			}
			selection, err := f.h.applyTemplateDefaults(&req, templateHardware{NodeID: f.node.ID})
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), req.ModelID, &selection, req.TemplateID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != f.shared.ID || selection.Weight.Revision != f.shared.Revision {
				t.Fatalf("selected model %d@%s became %d@%s", f.shared.ID, f.shared.Revision, got.ID, selection.Weight.Revision)
			}
		})
	}
}

func TestTemplateExplicitPrimaryUsesSelectedMirror(t *testing.T) {
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, true, false)
	selection := templateSelection{
		NodeID: f.node.ID, Model: f.template,
		Spec:   &store.InferenceSpecRow{Engine: "vllm", WeightSources: `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"},{"provider":"huggingface","repo_id":"Mirror/Deploy-FP8"}]`},
		Weight: weightSource{Provider: "huggingface", RepoID: "Mirror/Deploy-FP8", Revision: "template-rev"},
		Components: []deploy.ComponentBinding{{Name: "main", Role: "primary", OriginalModelID: f.template.ModelID,
			Provider: "huggingface", RepoID: "Mirror/Deploy-FP8", Revision: "template-rev", MountPath: "/model"}},
	}
	got, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), f.shared.ID, &selection, f.template.ID)
	if err != nil {
		t.Fatal(err)
	}
	primary, _ := deploy.PrimaryComponent(selection.Components)
	if got.ID != f.shared.ID || primary.StoreModelID != got.ID || primary.Provider != got.Source || primary.RepoID != got.SourceID || primary.Revision != got.Revision {
		t.Fatalf("primary component replaced selected artifact: model=%+v primary=%+v", got, primary)
	}
}

func TestTemplateAutomaticModelRejectsFailedArtifact(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, withNode := range []bool{false, true} {
			t.Run(fmt.Sprintf("provider=%t/node=%t", explicit, withNode), func(t *testing.T) {
				f, bad := templateFailedRevisionFixture(t, false)
				// Remove the other registrations so a failed exact match cannot hide
				// behind another candidate, or be upserted again as a new download.
				for _, model := range []*store.Model{f.hf, f.shared} {
					if err := f.h.d.Store.DeleteModel(model.ID); err != nil {
						t.Fatal(err)
					}
				}
				nodeID := uint(0)
				if withNode {
					nodeID = f.node.ID
				}
				got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: bad.Source, RepoID: bad.SourceID, Revision: bad.Revision}, f.template, "vllm", "", 0, nodeID, nil, explicit)
				if err == nil || got != nil {
					t.Fatalf("failed artifact accepted: model=%+v err=%v", got, err)
				}
				models, err := f.h.d.Store.ListModels()
				if err != nil || len(models) != 1 || models[0].ID != bad.ID {
					t.Fatalf("failure must not recreate a model: models=%+v err=%v", models, err)
				}
			})
		}
	}
}

func TestTemplateAutomaticModelPrefersReadableRevision(t *testing.T) {
	f, bad := templateFailedRevisionFixture(t, true)
	got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: bad.Source, RepoID: bad.SourceID, Revision: bad.Revision}, f.template, "vllm", "", 0, f.node.ID, nil)
	if err != nil || got == nil || got.ID != f.shared.ID {
		t.Fatalf("readable revision lost to failed exact match: model=%+v err=%v", got, err)
	}
}

func TestTemplateAutomaticModelPreservesAcquisitionStates(t *testing.T) {
	for _, state := range []string{"", proto.MFPending, proto.MFDownloading, proto.MFPresent} {
		t.Run("state="+state, func(t *testing.T) {
			f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, false, false)
			if state != "" {
				if _, err := f.h.d.Store.UpsertModelFile(&store.ModelFile{ModelID: f.hf.ID, StorageID: f.nfs.ID, Path: "Acme/hf", State: state}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: f.hf.Source, RepoID: f.hf.SourceID, Revision: f.hf.Revision}, f.template, "vllm", "", 0, f.node.ID, nil, true)
			if err != nil || got.ID != f.hf.ID {
				t.Fatalf("normal acquisition state %q rejected: model=%+v err=%v", state, got, err)
			}
		})
	}
}

func TestTemplateAutomaticModelUsesOtherViableCopy(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		withNode    bool
		pinFailed   bool
	}{
		{"ready local copy", proto.MFPresent, true, false},
		{"pending local copy", proto.MFPending, true, false},
		{"ready copy before node selection", proto.MFPresent, false, false},
		{"pin excludes healthy copy", proto.MFPresent, true, true},
		{"pin before node selection", proto.MFPresent, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, model := templateFailedRevisionFixture(t, false)
			if _, err := f.h.d.Store.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: f.h.d.Store.DefaultLocalStorageID(), NodeID: f.node.ID, Path: "/models/other-copy", State: tc.state}); err != nil {
				t.Fatal(err)
			}
			nodeID := uint(0)
			if tc.withNode {
				nodeID = f.node.ID
			}
			var pins []uint
			if tc.pinFailed {
				pins = []uint{f.nfs.ID}
			}
			got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: model.Source, RepoID: model.SourceID, Revision: model.Revision}, f.template, "vllm", "", 0, nodeID, pins, true)
			if tc.pinFailed {
				if err == nil || got != nil {
					t.Fatalf("excluded copy admitted failed artifact: model=%+v err=%v", got, err)
				}
			} else if err != nil || got.ID != model.ID {
				t.Fatalf("one failed location rejected a viable model: model=%+v err=%v", got, err)
			}
		})
	}
}

func TestTemplateAutomaticModelReadFailureDoesNotRegister(t *testing.T) {
	f, model := templateFailedRevisionFixture(t, true)
	failure := errors.New("model-file observation unavailable")
	const callback = "test:model-selection-file-read-failure"
	if err := f.h.d.Store.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "model_files" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.h.d.Store.DB().Callback().Query().Remove(callback) })
	before, err := f.h.d.Store.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []uint{0, f.node.ID} {
		got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: model.Source, RepoID: model.SourceID, Revision: "not-registered"}, f.template, "vllm", "", 0, nodeID, nil, true)
		if !errors.Is(err, failure) || got != nil {
			t.Fatalf("failed observation became a new registration: model=%+v err=%v", got, err)
		}
	}
	after, err := f.h.d.Store.ListModels()
	if err != nil || len(after) != len(before) {
		t.Fatalf("query failure changed model registrations: before=%d after=%d err=%v", len(before), len(after), err)
	}
}

func TestTemplateModelSelectionHTTP(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			f, _ := templateFailedRevisionFixture(t, true)
			if err := f.h.d.Store.DB().Create(&store.GPU{NodeID: f.node.ID, Index: 0, UUID: "GPU-selection", Name: "NVIDIA H100", Vendor: "nvidia", ComputeCapability: "9.0", ArchID: "sm_90", MemoryTotal: 80 << 30}).Error; err != nil {
				t.Fatal(err)
			}
			f.h.d.Online = func(uuid string) bool { return uuid == f.node.UUID }
			provider := "modelscope"
			if conflict {
				provider = "huggingface"
			}
			body := fmt.Sprintf(`{"name":"selected-model","model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":%q,"model_source":{"provider":%q,"storage_id":%d},"quick_params":{},"placement":{}}`, f.shared.ID, f.template.ID, f.node.UUID, provider, f.nfs.ID)
			status, raw := evaluateForFieldErrors(t, f.h, body)
			if status != http.StatusOK {
				t.Fatalf("evaluate status=%d body=%v", status, raw)
			}
			if conflict {
				errs := fieldErrorsOf(t, raw)
				if string(raw["ok"]) != "false" || len(errs) != 1 || errs[0].Field != "model_id" || !strings.Contains(fmt.Sprint(errs[0].Params), "provider") {
					t.Fatalf("provider conflict not explained: errors=%s", raw["errors"])
				}
			} else if string(raw["ok"]) != "true" || !strings.Contains(string(raw["acquisition"]), `"weights_present":true`) {
				t.Fatalf("evaluation lost the readable selected artifact: ok=%s acquisition=%s", raw["ok"], raw["acquisition"])
			}
			rec := httptest.NewRecorder()
			f.h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if conflict {
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("conflicting selection accepted: %d %s", rec.Code, rec.Body.String())
				}
				rows, err := f.h.d.Store.ListDeployments()
				if err != nil || len(rows) != 0 {
					t.Fatalf("conflicting request created a deployment: rows=%+v err=%v", rows, err)
				}
				return
			}
			if rec.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
			}
			var dep dto.Deployment
			if err := json.Unmarshal(rec.Body.Bytes(), &dep); err != nil {
				t.Fatal(err)
			}
			stored, err := f.h.d.Store.GetDeployment(dep.ID)
			if err != nil || stored.ModelID != f.shared.ID || !strings.Contains(stored.CatalogSelectionSnapshot, f.shared.Revision) {
				t.Fatalf("create persisted a substituted model: deployment=%+v err=%v", stored, err)
			}
		})
	}
}
