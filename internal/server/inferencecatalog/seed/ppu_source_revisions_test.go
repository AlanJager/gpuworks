package seed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type auditedPPUSources struct {
	ModelID  string                          `json:"model_id"`
	SpecName string                          `json:"spec_name"`
	Sources  []inferencecatalog.WeightSource `json:"sources"`
}

func loadAuditedPPUSources(t *testing.T) []auditedPPUSources {
	t.Helper()
	raw, err := os.ReadFile("testdata/ppu_source_revisions.json")
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Models []auditedPPUSources `json:"models"`
	}
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	if len(evidence.Models) == 0 {
		t.Fatal("provider revision evidence is empty")
	}
	return evidence.Models
}

func TestPPUWeightSourcesMatchPublishedRevisions(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range loadAuditedPPUSources(t) {
		t.Run(evidence.ModelID+"/"+evidence.SpecName, func(t *testing.T) {
			for _, model := range catalog.Models {
				if model.ModelID != evidence.ModelID {
					continue
				}
				for _, spec := range model.Specs {
					if spec.Name != evidence.SpecName {
						continue
					}
					if !reflect.DeepEqual(spec.Weights.Sources, evidence.Sources) {
						t.Errorf("provider revisions differ from independently verified source records:\ngot  %+v\nwant %+v", spec.Weights.Sources, evidence.Sources)
					}
					return
				}
			}
			t.Fatal("audited PPU template is missing")
		})
	}
}

func TestPPURevisionCorrectionUpdatesUpstreamAndPreservesUserSpec(t *testing.T) {
	evidence := loadAuditedPPUSources(t)[0]
	st, err := store.Open(filepath.Join(t.TempDir(), "seed.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := reconcileEmbeddedCatalog(st); err != nil {
		t.Fatal(err)
	}
	model, err := st.InferenceModelByModelID(evidence.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	var oldSpec store.InferenceSpecRow
	for _, spec := range model.Specs {
		if spec.Name == evidence.SpecName && spec.Source == "upstream" {
			oldSpec = spec
			break
		}
	}
	if oldSpec.ID == 0 {
		t.Fatal("audited upstream spec is missing")
	}
	oldSources := append([]inferencecatalog.WeightSource(nil), evidence.Sources...)
	for i := range oldSources {
		oldSources[i].Revision = evidence.Sources[0].Revision
	}
	oldJSON, err := json.Marshal(oldSources)
	if err != nil {
		t.Fatal(err)
	}
	oldSpec.WeightSources = string(oldJSON)
	// This row represents a previous catalog import, before the correction.
	if err := st.DB().Model(&store.InferenceSpecRow{}).Where("id = ?", oldSpec.ID).Update("weight_sources", oldSpec.WeightSources).Error; err != nil {
		t.Fatal(err)
	}
	userSpec := oldSpec
	userSpec.ID, userSpec.Source = 0, "user"
	if err := st.UpsertInferenceSpec(&userSpec); err != nil {
		t.Fatal(err)
	}
	if err := reconcileEmbeddedCatalog(st); err != nil {
		t.Fatal(err)
	}
	updated, err := st.InferenceModelByModelID(evidence.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	foundUpstream, foundUser := false, false
	for _, spec := range updated.Specs {
		if spec.Name != evidence.SpecName {
			continue
		}
		switch spec.Source {
		case "upstream":
			foundUpstream = true
			got, err := inferencecatalog.DecodeWeightSources(spec.WeightSources)
			if err != nil {
				t.Fatal(err)
			}
			if spec.ID != oldSpec.ID || !reflect.DeepEqual(got, evidence.Sources) {
				t.Errorf("upstream spec was not corrected in place: id=%d sources=%+v", spec.ID, got)
			}
		case "user":
			foundUser = true
			if spec.ID != userSpec.ID || spec.WeightSources != string(oldJSON) {
				t.Errorf("catalog update changed the user's spec: %+v", spec)
			}
		}
	}
	if !foundUpstream || !foundUser {
		t.Fatalf("upstream present=%v, user spec present=%v", foundUpstream, foundUser)
	}
}
