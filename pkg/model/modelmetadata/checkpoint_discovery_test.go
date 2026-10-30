package modelmetadata

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCheckpointDiscoveryWithoutFilenamePrefix(t *testing.T) {
	for _, ext := range []string{"safetensors", "bin", "pt"} {
		for _, prefix := range []string{"quant_model_weights", "custom-export.v2", "arbitrary"} {
			for _, indexed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/indexed=%v", ext, prefix, indexed), func(t *testing.T) {
					files := map[string]string{"config.json": `{"model_type":"example"}`}
					var want []string
					for i := 1; i <= 10; i++ {
						name := fmt.Sprintf("%s-%05d-of-00010.%s", prefix, i, ext)
						files[name] = "fixture weights"
						want = append(want, name)
					}
					if indexed {
						var pairs []string
						for i, name := range want {
							pairs = append(pairs, fmt.Sprintf("%q:%q", fmt.Sprint(i), name))
						}
						files[prefix+"."+ext+".index.json"] = `{"weight_map":{` + strings.Join(pairs, ",") + `}}`
					}
					r, err := Parse(context.Background(), Input{}, memory(files))
					if err != nil || len(r.Entries) != 1 || r.Entries[0].Incomplete || !slices.Equal(r.Entries[0].Files, want) {
						t.Fatalf("custom checkpoint not recognized: report=%+v err=%v", r, err)
					}
					delete(files, want[9])
					r, err = Parse(context.Background(), Input{}, memory(files))
					if err != nil || len(r.Entries) != 1 || !r.Entries[0].Incomplete || !slices.Contains(r.Entries[0].Missing, want[9]) {
						t.Fatalf("missing last shard not detected: report=%+v err=%v", r, err)
					}
				})
			}
		}
	}
}

func TestCheckpointDiscoveryUsesIndexAndFormatEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   map[string]string
		want    []string
		missing string
		issue   string
		noEntry bool
	}{
		{name: "single custom safetensors", files: map[string]string{"export.safetensors": "weights"}, want: []string{"export.safetensors"}},
		{name: "safetensors precedes alternate bin", files: map[string]string{"model.safetensors": "weights", "pytorch_model.bin": "alternative"}, want: []string{"model.safetensors"}},
		{name: "index excludes consolidated duplicate", files: map[string]string{"custom.safetensors.index.json": `{"weight_map":{"x":"parts/a.safetensors"}}`, "parts/a.safetensors": "weights", "model.safetensors": "duplicate"}, want: []string{"parts/a.safetensors"}},
		{name: "custom index missing shard cannot use loose weights", files: map[string]string{"custom.safetensors.index.json": `{"weight_map":{"x":"absent.safetensors"}}`, "model.safetensors": "unrelated"}, want: []string{"absent.safetensors"}, missing: "absent.safetensors"},
		{name: "malformed custom index cannot use loose weights", files: map[string]string{"custom.safetensors.index.json": `{"weight_map":`, "model.safetensors": "unrelated"}, issue: "custom.safetensors.index.json", noEntry: true},
		{name: "same shard set in alias indexes", files: map[string]string{"custom.safetensors.index.json": `{"weight_map":{"x":"part.safetensors"}}`, "alias.safetensors.index.json": `{"weight_map":{"x":"part.safetensors"}}`, "part.safetensors": "weights"}, want: []string{"part.safetensors"}},
		{name: "conflicting nonstandard indexes cannot silently choose one", files: map[string]string{"custom.safetensors.index.json": `{"weight_map":{"x":"a.safetensors"}}`, "alternate.safetensors.index.json": `{"weight_map":{"x":"b.safetensors"}}`, "a.safetensors": "one", "b.safetensors": "two"}, issue: "checkpoint indexes", noEntry: true},
		{name: "all unindexed shard groups checked", files: map[string]string{"model-a-00001-of-00001.safetensors": "weights", "model-b-00001-of-00002.safetensors": "weights"}, missing: "model-b-00002-of-00002.safetensors"},
		{name: "zero shard count", files: map[string]string{"model-00000-of-00000.safetensors": "weights"}, issue: "shard", noEntry: true},
		{name: "out of range shard number", files: map[string]string{"model-00001-of-00001.safetensors": "weights", "model-00002-of-00001.safetensors": "weights"}, issue: "shard", noEntry: true},
		{name: "inconsistent shard counts", files: map[string]string{"model-00001-of-00002.safetensors": "weights", "model-00002-of-00002.safetensors": "weights", "model-00001-of-00003.safetensors": "weights"}, issue: "inconsistent", noEntry: true},
		{name: "shard expansion bounded across groups", files: map[string]string{"first-00001-of-06000.safetensors": "weights", "second-00001-of-06000.safetensors": "weights"}, issue: "metadata read limit", noEntry: true},
		{name: "training files alone", files: map[string]string{"optimizer.pt": "training", "scheduler.pt": "training", "scaler.pt": "training", "training_args.bin": "training"}, noEntry: true},
		{name: "pt checkpoint with training files", files: map[string]string{"weights.pt": "weights", "optimizer.pt": "training", "scheduler.pt": "training"}, want: []string{"weights.pt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.files["config.json"] = `{"model_type":"example"}`
			r, err := Parse(context.Background(), Input{}, memory(tc.files))
			if err != nil {
				t.Fatal(err)
			}
			if tc.noEntry {
				if len(r.Entries) != 0 {
					t.Fatalf("invalid checkpoint accepted: %+v", r)
				}
			} else {
				if len(r.Entries) != 1 {
					t.Fatalf("checkpoint not recognized: %+v", r)
				}
				e := r.Entries[0]
				if e.Incomplete != (tc.missing != "") || (tc.missing != "" && !slices.Contains(e.Missing, tc.missing)) {
					t.Fatalf("completeness=%+v", e)
				}
				if tc.want != nil && !slices.Equal(e.Files, tc.want) {
					t.Fatalf("files=%v want=%v", e.Files, tc.want)
				}
			}
			if tc.issue != "" && !slices.ContainsFunc(r.Issues, func(i Issue) bool { return strings.Contains(i.Path+" "+i.Detail, tc.issue) }) {
				t.Fatalf("missing diagnostic %q: %+v", tc.issue, r)
			}
		})
	}
}

func TestCustomCheckpointNamesRetainOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		files        map[string]string
	}{
		{"modelscope", "modelscope", map[string]string{"configuration.json": `{"task":"text-generation","model":{"type":"example"}}`, "export.pt": "weights"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(context.Background(), Input{}, memory(tc.files))
			if err != nil || len(r.Entries) != 1 || r.Entries[0].Incomplete || r.Entries[0].Format != tc.format {
				t.Fatalf("report=%+v err=%v", r, err)
			}
		})
	}
	files := memory(map[string]string{"encoder/config.json": `{"model_type":"example"}`, "encoder/custom.safetensors": "weights"})
	r, err := Parse(context.Background(), Input{Path: "encoder", Context: Context{Repository: true, Components: []Reference{{Path: "encoder", Role: "component"}}}}, files)
	if err != nil || !r.Component || len(r.Entries) != 0 {
		t.Fatalf("component with custom weights became independent: %+v %v", r, err)
	}
}
