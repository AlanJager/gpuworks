package modelscan

import (
	"context"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestScanDualDiffusersManifestsKeepComponentsAndIndependentEntries(t *testing.T) {
	root := t.TempDir()
	repo := "org/pipeline"
	write(t, p(root, repo+"/README.md"), "---\npipeline_tag: image-to-video\n---\n")
	write(t, p(root, repo+"/modular_model_index.json"), `{"_class_name":"ExampleModularPipeline","transformer":["transformers","ExampleModel"]}`)
	write(t, p(root, repo+"/model_index.json"), `{"_class_name":"ExamplePipeline","transformer":["transformers","ExampleModel"],"vision_encoder":["transformers","Qwen3VLVisionModel"],"variant":["custom","Pipeline"]}`)
	hfModel(t, root, repo+"/transformer")
	write(t, p(root, repo+"/vision_encoder/config.json"), `{"architectures":["Qwen3VLVisionModel"],"model_type":"qwen3_vl"}`)
	write(t, p(root, repo+"/vision_encoder/model.safetensors"), "placeholder weights")
	write(t, p(root, repo+"/variant/model_index.json"), `{"_class_name":"VariantPipeline","encoder":["transformers","ExampleModel"]}`)
	hfModel(t, root, repo+"/variant/encoder")
	hfModel(t, root, repo+"/standalone")
	full, err := Scan(context.Background(), root, Options{MaxDepth: 8})
	if err != nil {
		t.Fatal(err)
	}
	candidates := byRel(full)
	if len(candidates) != 3 {
		t.Errorf("unexpected models: %+v", full.Candidates)
	}
	for _, rel := range []string{repo, repo + "/variant", repo + "/standalone"} {
		if c, ok := candidates[rel]; !ok || c.Incomplete {
			t.Errorf("valid entry hidden or made incomplete: %s %+v", rel, c)
		}
	}
	for _, rel := range []string{repo + "/transformer", repo + "/vision_encoder", repo + "/variant/encoder"} {
		if _, found := candidates[rel]; found || skippedMap(full)[rel] != proto.ScanSkipComponent {
			t.Errorf("component not excluded: %s %+v", rel, full)
		}
		report, err := ParseMetadataWithin(context.Background(), root, p(root, rel))
		if err != nil || !report.Component || len(report.Entries) != 0 || len(report.Tasks) != 0 {
			t.Errorf("import parsing lost ownership: %s %+v %v", rel, report, err)
		}
	}
	var paged []Candidate
	cursor := ""
	for i := 0; i < 10; i++ {
		page, err := Scan(context.Background(), root, Options{MaxDepth: 8, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		paged = append(paged, page.Candidates...)
		if page.Complete {
			break
		}
		if cursor == page.NextCursor {
			t.Fatal("pagination did not progress")
		}
		cursor = page.NextCursor
	}
	if !reflect.DeepEqual(full.Candidates, paged) {
		t.Fatalf("pagination changed entries: full=%+v paged=%+v", full.Candidates, paged)
	}
}
