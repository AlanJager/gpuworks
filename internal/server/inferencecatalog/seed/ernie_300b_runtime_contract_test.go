package seed

import (
	"slices"
	"testing"
)

func TestERNIE300BDeclaresBF16AndAnA100FittingProfile(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "baidu/ernie-4.5-300b-a47b-pt" {
			continue
		}
		specs := make(map[string]struct {
			cc   string
			args []string
		}, len(model.Specs))
		for _, spec := range model.Specs {
			specs[spec.Name] = struct {
				cc   string
				args []string
			}{cc: spec.GPUs.CC, args: spec.Args}
			if slices.Contains(spec.Args, "fp8") {
				t.Fatalf("%s forces FP8 although its checkpoint is BF16", spec.Name)
			}
		}
		a100, ok := specs["bf16-300b-vllm-nvidia-a100"]
		if !ok {
			t.Fatal("missing A100 BF16 profile")
		}
		if a100.cc != "8.0" {
			t.Fatalf("A100 profile cc=%q, want 8.0", a100.cc)
		}
		for flag, value := range map[string]string{
			"--max-model-len":          "4096",
			"--gpu-memory-utilization": "0.97",
		} {
			index := slices.Index(a100.args, flag)
			if index < 0 || index+1 >= len(a100.args) || a100.args[index+1] != value {
				t.Fatalf("A100 profile args=%v, want %s %s", a100.args, flag, value)
			}
		}
		if !slices.Contains(a100.args, "--enforce-eager") {
			t.Fatalf("A100 profile args=%v, want --enforce-eager", a100.args)
		}
		return
	}
	t.Fatal("ERNIE-4.5-300B model not found")
}
