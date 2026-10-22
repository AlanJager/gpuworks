package modelmetadata

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

func TestComponentFileContracts(t *testing.T) {
	for _, tc := range []struct {
		name, format, library, class string
		files                        map[string]string
		incomplete                   bool
		missing, issue               string
	}{
		{name: "dense complete", format: "sentence", class: "sentence_transformers.models.Dense", files: map[string]string{"config.json": "{}", "model.safetensors": "weights"}},
		{name: "dense bin", format: "sentence", class: "sentence_transformers.models.Dense", files: map[string]string{"config.json": "{}", "pytorch_model.bin": "weights"}},
		{name: "pooling", format: "sentence", class: "sentence_transformers.models.Pooling", files: map[string]string{"config.json": "{}"}},
		{name: "logit score", format: "sentence", class: "sentence_transformers.cross_encoder.modules.logit_score.LogitScore", files: map[string]string{"config.json": `{"true_token_id":1}`}},
		{name: "normalize without directory", format: "sentence", class: "sentence_transformers.models.Normalize"},
		{name: "custom normalize", format: "sentence", class: "custom.Normalize", incomplete: true, issue: "unverified_component"},
		{name: "custom weighted module", format: "sentence", class: "custom.Dense", files: map[string]string{"config.json": "{}", "model.safetensors": "weights"}, incomplete: true, issue: "unverified_component"},
		{name: "missing component directory", format: "sentence", class: "sentence_transformers.models.Dense", incomplete: true, missing: "component"},
		{name: "missing config", format: "sentence", class: "sentence_transformers.models.Dense", files: map[string]string{"model.safetensors": "weights"}, incomplete: true, missing: "component/config.json"},
		{name: "bad config", format: "sentence", class: "sentence_transformers.models.Dense", files: map[string]string{"config.json": "{", "model.safetensors": "weights"}, incomplete: true, issue: "metadata_unreadable"},
		{name: "diffusers complete", library: "diffusers", class: "FluxTransformer2DModel", files: map[string]string{"config.json": "{}", "diffusion_pytorch_model.safetensors": "weights"}},
		{name: "diffusers bin", library: "diffusers", class: "UNet2DConditionModel", files: map[string]string{"config.json": "{}", "diffusion_pytorch_model.bin": "weights"}},
		{name: "diffusers missing shard", library: "diffusers", class: "AutoencoderKL", files: map[string]string{"config.json": "{}", "diffusion_pytorch_model.safetensors.index.json": `{"weight_map":{"x":"a.safetensors","y":"b.safetensors"}}`, "a.safetensors": "weights", "diffusion_pytorch_model.bin": "unrelated"}, incomplete: true, missing: "component/b.safetensors"},
		{name: "diffusers complete shards", library: "diffusers", class: "AutoencoderKL", files: map[string]string{"config.json": "{}", "diffusion_pytorch_model.safetensors.index.json": `{"weight_map":{"x":"a.safetensors","y":"b.safetensors"}}`, "a.safetensors": "weights", "b.safetensors": "weights"}},
		{name: "diffusers unindexed missing shard", library: "diffusers", class: "AutoencoderKL", files: map[string]string{"config.json": "{}", "diffusion_pytorch_model-00001-of-00002.safetensors": "weights"}, incomplete: true, missing: "component/diffusion_pytorch_model-00002-of-00002.safetensors"},
		{name: "scheduler", library: "diffusers", class: "EulerDiscreteScheduler", files: map[string]string{"scheduler_config.json": "{}"}},
		{name: "scheduler missing config", library: "diffusers", class: "EulerDiscreteScheduler", files: map[string]string{"README.md": "doc"}, incomplete: true, missing: "component/scheduler_config.json"},
		{name: "text encoder", library: "transformers", class: "CLIPTextModelWithProjection", files: map[string]string{"config.json": "{}", "model.safetensors": "weights"}},
		{name: "clip tokenizer", library: "transformers", class: "CLIPTokenizer", files: map[string]string{"vocab.json": "{}", "merges.txt": "vocab"}},
		{name: "clip tokenizer missing merges", library: "transformers", class: "CLIPTokenizer", files: map[string]string{"vocab.json": "{}"}, incomplete: true, missing: "component/merges.txt"},
		{name: "fast tokenizer", library: "transformers", class: "CLIPTokenizerFast", files: map[string]string{"tokenizer.json": "{}"}},
		{name: "fast tokenizer fallback", library: "transformers", class: "T5TokenizerFast", files: map[string]string{"spiece.model": "vocab"}},
		{name: "image processor", library: "transformers", class: "CLIPImageProcessor", files: map[string]string{"preprocessor_config.json": "{}"}},
		{name: "custom diffusion component", library: "custom", class: "AudioVAE", files: map[string]string{"config.json": "{}", "model.safetensors": "weights"}, incomplete: true, issue: "unverified_component"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for name, body := range tc.files {
				files["component/"+name] = body
			}
			if tc.format == "sentence" {
				b, _ := json.Marshal([]map[string]string{{"path": "component", "type": tc.class}})
				files["modules.json"] = string(b)
			} else {
				b, _ := json.Marshal(map[string]any{"_class_name": "ExamplePipeline", "component": []string{tc.library, tc.class}})
				files["model_index.json"] = string(b)
			}
			report, err := Parse(context.Background(), Input{Path: "."}, memory(files))
			if err != nil || len(report.Entries) != 1 || report.Entries[0].Incomplete != tc.incomplete {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if tc.missing != "" && !slices.Contains(report.Entries[0].Missing, tc.missing) {
				t.Fatalf("missing cause not preserved: %+v", report)
			}
			if tc.issue != "" && !slices.ContainsFunc(report.Issues, func(i Issue) bool {
				return i.Code == tc.issue && i.Path == "component" || i.Code == tc.issue && i.Path == "component/config.json"
			}) {
				t.Fatalf("issue absent: %+v", report)
			}
		})
	}
}

type componentReadFailure struct {
	FileView
	name string
	err  error
}

func (v componentReadFailure) ReadRange(ctx context.Context, name string, offset, length int64) ([]byte, error) {
	if name == v.name {
		return nil, v.err
	}
	return v.FileView.ReadRange(ctx, name, offset, length)
}

func TestComponentIndexReadFailures(t *testing.T) {
	const index = "component/model.safetensors.index.json"
	for _, failure := range []error{fs.ErrPermission, ErrReadLimit, context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			files := componentReadFailure{FileView: memory(map[string]string{"modules.json": `[{"path":"component","type":"sentence_transformers.models.Transformer"}]`, "component/config.json": "{}", index: `{"weight_map":{"a":"a.safetensors"}}`}), name: index, err: failure}
			report, err := Parse(context.Background(), Input{Path: "."}, files)
			if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
				if !errors.Is(err, failure) {
					t.Fatalf("cancellation lost: %v", err)
				}
				return
			}
			if err != nil || len(report.Entries) != 1 || !report.Entries[0].Incomplete || len(report.Entries[0].Missing) != 0 {
				t.Fatalf("read error misreported as missing files: %+v %v", report, err)
			}
			code := "metadata_unreadable"
			if errors.Is(failure, ErrReadLimit) {
				code = "metadata_read_limit"
			}
			if !slices.ContainsFunc(report.Issues, func(i Issue) bool {
				return i.Path == index && i.Code == code && strings.Contains(i.Detail, failure.Error())
			}) {
				t.Fatalf("source/cause lost: %+v", report)
			}
		})
	}
}
