package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// The index is built once per catalog version: asked again with nothing
// changed it is not rebuilt, and adding, updating or deleting a spec rebuilds
// it from the new rows.
func TestWeightRepoIndexRebuildsOnlyWhenTheCatalogChanges(t *testing.T) {
	st, err := Open("file:weight_repo_index?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	builds := 0
	build := func(specs []InferenceSpecWeightSources) map[string]uint {
		builds++
		out := map[string]uint{}
		for _, spec := range specs {
			out[spec.WeightSources] = spec.ModelID
		}
		return out
	}
	ask := func(wantBuilds int, wantKeys ...string) {
		t.Helper()
		index, err := st.WeightRepoIndex(build)
		if err != nil {
			t.Fatal(err)
		}
		if builds != wantBuilds || len(index) != len(wantKeys) {
			t.Fatalf("builds=%d index=%v, want %d builds and keys %v", builds, index, wantBuilds, wantKeys)
		}
		for _, key := range wantKeys {
			if _, ok := index[key]; !ok {
				t.Fatalf("index %v lacks %s", index, key)
			}
		}
	}
	ask(1)
	ask(1)
	spec := InferenceSpecRow{ModelID: 7, Name: "a", Image: "img", WeightSources: "repo-a", GPUs: "{}"}
	if err := st.DB().Create(&spec).Error; err != nil {
		t.Fatal(err)
	}
	ask(2, "repo-a")
	ask(2, "repo-a")
	if err := st.DB().Model(&InferenceSpecRow{}).Where("id = ?", spec.ID).Update("weight_sources", "repo-b").Error; err != nil {
		t.Fatal(err)
	}
	ask(3, "repo-b")
	if err := st.DB().Delete(&InferenceSpecRow{}, spec.ID).Error; err != nil {
		t.Fatal(err)
	}
	ask(4)
}
