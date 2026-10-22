package modelscan

import (
	"context"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestScanAndParseMetadataKeepDeepSiblingContextsSeparate(t *testing.T) {
	root := t.TempDir()
	repo := "deep/group/repository"
	write(t, p(root, repo+"/README.md"), "---\npipeline_tag: feature-extraction\n---\n")
	expected := map[string]string{}
	for _, branch := range []struct{ name, task string }{{"left", "text-to-image"}, {"right", "image-to-image"}} {
		dir := repo + "/" + branch.name
		write(t, p(root, dir+"/README.md"), "---\npipeline_tag: "+branch.task+"\n---\n")
		write(t, p(root, dir+"/model_index.json"), `{"_class_name":"FluxPipeline","transformer":["diffusers","FluxTransformer2DModel"]}`)
		write(t, p(root, dir+"/transformer/config.json"), `{"_class_name":"FluxTransformer2DModel"}`)
		write(t, p(root, dir+"/transformer/diffusion_pytorch_model.safetensors"), "weights")
		hfModel(t, root, dir+"/variants/v1")
		expected[dir] = branch.task
		expected[dir+"/variants/v1"] = branch.task
	}
	hfModel(t, root, repo+"/unchanged/deep/entry")
	expected[repo+"/unchanged/deep/entry"] = "feature-extraction"
	hfModel(t, root, "deep/group/other")
	result, err := Scan(context.Background(), root, Options{MaxDepth: 12})
	if err != nil {
		t.Fatal(err)
	}
	candidates := byRel(result)
	if len(candidates) != len(expected)+1 {
		t.Fatalf("unexpected entries: %+v", result)
	}
	for rel, task := range expected {
		candidate, ok := candidates[rel]
		if !ok || candidate.Incomplete || !slices.Equal(candidate.Tasks, []string{task}) {
			t.Fatalf("scan context for %s: %+v", rel, candidate)
		}
		report, err := ParseMetadataWithin(context.Background(), root, p(root, rel))
		if err != nil || len(report.Entries) != 1 || report.Entries[0].Incomplete || !slices.Equal(report.Tasks, candidate.Tasks) || report.Entries[0].Format != candidate.Format {
			t.Fatalf("scan/metadata parsing disagree for %s: %+v %v", rel, report, err)
		}
	}
	for _, branch := range []string{"left", "right"} {
		rel := repo + "/" + branch + "/transformer"
		report, err := ParseMetadataWithin(context.Background(), root, p(root, rel))
		if err != nil || !report.Component || len(report.Entries) != 0 || skippedMap(result)[rel] != proto.ScanSkipComponent {
			t.Fatalf("ownership lost for %s: %+v %v", rel, report, err)
		}
	}
	other := candidates["deep/group/other"]
	report, err := ParseMetadataWithin(context.Background(), root, p(root, "deep/group/other"))
	if err != nil || len(other.Tasks) != 0 || len(report.Tasks) != 0 {
		t.Fatalf("context leaked to sibling repository: %+v %+v %v", other, report, err)
	}
}
