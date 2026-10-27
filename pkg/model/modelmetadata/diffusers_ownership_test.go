package modelmetadata

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"slices"
	"testing"
)

func modularOwnershipFiles() map[string]string {
	return map[string]string{
		"modular_model_index.json":                        `{"_class_name":"ExampleModularPipeline","transformer":["diffusers","ExampleModel"]}`,
		"transformer/config.json":                         `{"_class_name":"ExampleModel"}`,
		"transformer/diffusion_pytorch_model.safetensors": "weights",
	}
}

func TestDiffusersSupplementalOwnershipPreservesSelectedEntry(t *testing.T) {
	files := modularOwnershipFiles()
	before, err := Parse(context.Background(), Input{}, memory(files))
	if err != nil || len(before.Entries) != 1 || before.Entries[0].Incomplete {
		t.Fatalf("invalid complete fixture: %+v %v", before, err)
	}
	files["model_index.json"] = `{
		"_class_name":"ExamplePipeline",
		"transformer":["custom","DifferentClass"],
		"vision_encoder":["transformers","Qwen3VLVisionModel"],
		"unused":["custom","UnknownComponent"]
	}`
	files["vision_encoder/config.json"] = `{"architectures":["Qwen3VLVisionModel"],"model_type":"qwen3_vl"}`
	files["vision_encoder/model.safetensors"] = "weights"
	after, err := Parse(context.Background(), Input{}, memory(files))
	if err != nil || !reflect.DeepEqual(before.Entries, after.Entries) || !reflect.DeepEqual(before.Issues, after.Issues) {
		t.Fatalf("supplemental declarations changed selected entry: before=%+v after=%+v err=%v", before, after, err)
	}
	want := []Reference{
		{Path: "transformer", Role: "component", Source: "modular_model_index.json"},
		{Path: "unused", Role: "component", Source: "model_index.json"},
		{Path: "vision_encoder", Role: "component", Source: "model_index.json"},
	}
	if !reflect.DeepEqual(after.Components, want) {
		t.Fatalf("ownership=%+v want=%+v", after.Components, want)
	}
	child, err := Parse(context.Background(), Input{Path: "vision_encoder", Context: Context{Repository: true, Components: after.Components}}, memory(files))
	if err != nil || !child.Component || len(child.Entries) != 0 {
		t.Fatalf("component offered as independent model: %+v %v", child, err)
	}
	delete(files, "transformer/diffusion_pytorch_model.safetensors")
	missing, err := Parse(context.Background(), Input{}, memory(files))
	if err != nil || len(missing.Entries) != 1 || !missing.Entries[0].Incomplete {
		t.Fatalf("supplemental weights hid selected entry's missing weights: %+v %v", missing, err)
	}
}

func TestDiffusersSupplementalOwnershipOnlyClaimsLocalComponents(t *testing.T) {
	for _, tc := range []struct {
		name, declaration string
		wantPath          string
		wantIssue         bool
	}{
		{"local", `["transformers","ExampleModel"]`, "repo/encoder", false},
		{"remote repo", `["transformers","ExampleModel",{"repo":"org/other","subfolder":"encoder"}]`, "", false},
		{"remote loading spec", `["transformers","ExampleModel",{"pretrained_model_name_or_path":"org/other","subfolder":"encoder"}]`, "", false},
		{"local subfolder", `["transformers","ExampleModel",{"subfolder":"parts/encoder"}]`, "repo/parts/encoder", false},
		{"escaping subfolder", `["transformers","ExampleModel",{"subfolder":"../other"}]`, "", true},
		{"root subfolder", `["transformers","ExampleModel",{"subfolder":"."}]`, "", true},
		{"optional", `[null,null]`, "", false},
		{"scalar setting", `true`, "", false},
		{"empty class", `["transformers",""]`, "", true},
		{"invalid loading spec", `["transformers","ExampleModel",7]`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for name, body := range modularOwnershipFiles() {
				files["repo/"+name] = body
			}
			files["repo/model_index.json"] = `{"_class_name":"OtherPipeline","encoder":` + tc.declaration + `}`
			report, err := Parse(context.Background(), Input{Path: "repo"}, memory(files))
			if err != nil || len(report.Entries) != 1 || report.Entries[0].Incomplete {
				t.Fatalf("selected entry lost: %+v %v", report, err)
			}
			wantCount := 1
			if tc.wantPath != "" {
				wantCount++
				if !slices.ContainsFunc(report.Components, func(r Reference) bool { return r.Path == tc.wantPath && r.Source == "model_index.json" }) {
					t.Errorf("missing local ownership %s: %+v", tc.wantPath, report.Components)
				}
			}
			if len(report.Components) != wantCount {
				t.Errorf("unexpected ownership: %+v", report.Components)
			}
			foundIssue := slices.ContainsFunc(report.Issues, func(i Issue) bool { return i.Path == "repo/model_index.json" })
			if foundIssue != tc.wantIssue {
				t.Errorf("diagnostic=%t want=%t issues=%+v", foundIssue, tc.wantIssue, report.Issues)
			}
		})
	}
}

func TestDiffusersSupplementalManifestErrorsPreserveSelectedEntry(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `{"_class_name":7}`, `{"_class_name":"Other","../outside":["transformers","Model"]}`} {
		files := modularOwnershipFiles()
		files["model_index.json"] = body
		r, err := Parse(context.Background(), Input{}, memory(files))
		if err != nil || len(r.Entries) != 1 || r.Entries[0].Incomplete || len(r.Components) != 1 {
			t.Fatalf("invalid supplement changed selected entry: %+v %v", r, err)
		}
		if !slices.ContainsFunc(r.Issues, func(i Issue) bool { return i.Path == "model_index.json" }) {
			t.Errorf("supplement error not reported for %s: %+v", body, r.Issues)
		}
	}
	for _, failure := range []error{fs.ErrPermission, ErrReadLimit, context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			files := modularOwnershipFiles()
			files["model_index.json"] = `{"_class_name":"OtherPipeline","encoder":["transformers","Model"]}`
			view := componentReadFailure{FileView: memory(files), name: "model_index.json", err: failure}
			r, err := Parse(context.Background(), Input{}, view)
			if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
				if !errors.Is(err, failure) {
					t.Fatalf("cancellation swallowed: %v", err)
				}
				return
			}
			if err != nil || len(r.Entries) != 1 || r.Entries[0].Incomplete {
				t.Fatalf("selected entry lost: %+v %v", r, err)
			}
			if !slices.ContainsFunc(r.Issues, func(i Issue) bool { return i.Path == "model_index.json" && i.Detail != "" }) {
				t.Fatalf("read failure lost: %+v", r.Issues)
			}
		})
	}
}
