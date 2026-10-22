package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestReferencedCatalogPackageDigestsIncludesCatalogDeploymentAndInstance(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "catalog-package-refs.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	modelDigest := strings.Repeat("a", 64)
	deploymentDigest := strings.Repeat("b", 64)
	instanceDigest := strings.Repeat("c", 64)
	specDigest := strings.Repeat("d", 64)
	model := InferenceModel{ModelID: "gpuworks/package-ref", Source: "upstream", PackageDigest: strings.ToUpper(modelDigest)}
	if err := st.DB().Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&InferenceSpecRow{ModelID: model.ID, Source: "user", Name: "user-assets", PackageDigest: specDigest, Engine: "vllm", Image: "image", Quantization: "bf16", WeightSources: `[]`, GPUs: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(map[string]any{"runtime_assets": []proto.RuntimeAsset{{File: "assets/deployment.py", PackageDigest: deploymentDigest}}})
	deployment, err := st.CreateDeployment(&Deployment{Name: "package-ref", CatalogSelectionSnapshot: string(snapshot)})
	if err != nil {
		t.Fatal(err)
	}
	desired, _ := json.Marshal(proto.InstanceDesiredState{UUID: "package-ref-instance", ResolvedLaunch: proto.ResolvedLaunch{RuntimeAssets: []proto.RuntimeAsset{{File: "assets/instance.py", PackageDigest: instanceDigest}}}})
	if _, err := st.CreateInstance(&Instance{UUID: "package-ref-instance", DeploymentID: deployment.ID, State: proto.InstPending, DesiredStateJSON: string(desired)}); err != nil {
		t.Fatal(err)
	}
	// Malformed historical JSON and invalid digests are ignored without
	// weakening the valid references that keep executable snapshots alive.
	if _, err := st.CreateDeployment(&Deployment{Name: "malformed-package-ref", CatalogSelectionSnapshot: "{"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&InferenceModel{ModelID: "gpuworks/invalid-ref", Source: "user", PackageDigest: "not-a-digest"}).Error; err != nil {
		t.Fatal(err)
	}
	got, err := st.ReferencedCatalogPackageDigests()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{modelDigest, deploymentDigest, instanceDigest, specDigest}; !reflect.DeepEqual(got, want) {
		t.Fatalf("references=%v want=%v", got, want)
	}
}
