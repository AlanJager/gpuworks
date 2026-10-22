package store

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestCommunitySeriesCandidatesSurviveTagUpsertWhenOmitted(t *testing.T) {
	st, err := Open("file:image_series_common_preserve_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal([]map[string]string{{"flag": "--trust-remote-code"}})
	env, _ := json.Marshal([]map[string]string{{"key": "HF_HUB_ENABLE_HF_TRANSFER", "value": "1"}})
	series, err := st.UpsertImageSeries(&ImageSeries{Name: "community/runtime", Engine: "vllm", Framework: "cuda", Source: "community", Enabled: true, CommonArgs: string(args), CommonEnv: string(env)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&Image{SeriesID: series.ID, ImageRef: "community/runtime:v1", Source: "community", Engine: "vllm", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	updated, err := st.UpsertImageSeries(&ImageSeries{Name: "community/runtime", Engine: "vllm", Framework: "cuda", Source: "community", Enabled: true, CommonArgs: "[]", CommonEnv: "[]"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CommonArgs != string(args) || updated.CommonEnv != string(env) {
		t.Fatalf("candidates were cleared by omitted import: args=%s env=%s", updated.CommonArgs, updated.CommonEnv)
	}
}

func TestCommunityImagesAutoGroupByRepositoryAndFramework(t *testing.T) {
	st, err := Open("file:image_series_grouping_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.UpsertImage(&Image{ImageRef: "registry.example/team/runtime:v1", Source: "community", Engine: "vllm", Framework: "cuda", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.UpsertImage(&Image{ImageRef: "registry.example/team/runtime:v2", Source: "community", Engine: "vllm", Framework: "cann", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.SeriesID == second.SeriesID {
		t.Fatalf("different frameworks were grouped into series %d", first.SeriesID)
	}
	firstSeries, err := st.GetImageSeries(first.SeriesID)
	if err != nil {
		t.Fatal(err)
	}
	secondSeries, err := st.GetImageSeries(second.SeriesID)
	if err != nil {
		t.Fatal(err)
	}
	if firstSeries.Name != "team/runtime" || secondSeries.Name != "team/runtime · cann" {
		t.Fatalf("series names = %q, %q", firstSeries.Name, secondSeries.Name)
	}
}

func TestListEnabledImagesRequiresEnabledSeries(t *testing.T) {
	st, err := Open("file:image_enabled_series_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&ImageSeries{}).Where("1 = 1").Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&Image{}).Where("1 = 1").Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	series, err := st.UpsertImageSeries(&ImageSeries{Name: "disabled-series", Engine: "vllm", Framework: "cuda", Source: "official", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	enabledVersion, err := st.UpsertImage(&Image{SeriesID: series.ID, ImageRef: "gpustack/runner:enabled", Source: "official", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&Image{SeriesID: series.ID, ImageRef: "gpustack/runner:disabled", Source: "official", Engine: "vllm", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListEnabledImages()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("disabled series leaked enabled versions: %+v", rows)
	}
	if _, err := st.SetImageSeriesEnabled(series.ID, true); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListEnabledImages()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != enabledVersion.ID {
		t.Fatalf("enabled versions = %+v", rows)
	}
}
