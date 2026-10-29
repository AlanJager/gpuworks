package seed

import (
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func TestFunASRNanoUsesAllocatedTensorParallelism(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "funaudiollm/fun-asr-nano-2512-vllm" {
			continue
		}
		for _, spec := range model.Specs {
			for _, tp := range []int{1, 2, 4} {
				t.Run(spec.Name+"/tp"+strconv.Itoa(tp), func(t *testing.T) {
					materialized, err := deploy.Materialize(deploy.MaterializeInput{
						Engine: spec.Engine, TemplateArgs: spec.Args,
					})
					if err != nil {
						t.Fatal(err)
					}
					result := deploy.BuildVLLM(deploy.Params{
						ModelPath: "/model", TensorParallel: tp, Quick: materialized.Quick,
						TemplateArgs: materialized.TemplateResidual,
					})
					values := []string{}
					for i, arg := range result.Argv {
						if arg == "--tensor-parallel-size" && i+1 < len(result.Argv) {
							values = append(values, result.Argv[i+1])
						}
					}
					if len(values) != 1 || values[0] != strconv.Itoa(tp) {
						t.Fatalf("allocated TP=%d, launch TP values=%v", tp, values)
					}
				})
			}
		}
		return
	}
	t.Fatal("Fun-ASR Nano vLLM template is missing")
}
