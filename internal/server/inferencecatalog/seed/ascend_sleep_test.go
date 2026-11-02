package seed

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Every built-in Ascend vLLM template must launch with sleep enabled: the
// platform rewrites the template-layer settings sleep cannot run with
// (weight_nz_mode, expandable_segments) instead of refusing the deployment.
func TestBuiltinAscendVLLMTemplatesLaunchSleepable(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	engine, err := deploy.SelectEngine(deploy.EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	checked, rewroteAllocator := 0, 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if !strings.EqualFold(spec.GPUs.Vendor, "ascend") || spec.Engine != deploy.EngineVLLM {
				continue
			}
			where := model.ModelID + "/" + spec.Name
			checked++
			expandable := strings.Contains(spec.Env["PYTORCH_NPU_ALLOC_CONF"], "expandable_segments:True")
			if expandable {
				rewroteAllocator++
			}
			tp, pp, dp := statedParallelism(spec.Args, "--tensor-parallel-size"), statedParallelism(spec.Args, "--pipeline-parallel-size"), statedParallelism(spec.Args, "--data-parallel-size")
			launch, warnings, err := deploy.ResolveLaunchWithWarnings(deploy.ResolveLaunchInput{
				Engine: engine, Image: spec.Image, Entrypoint: spec.Entrypoint, APIContract: spec.API,
				AllowUnresolved: true,
				Params: deploy.Params{
					ModelPath: "/model", APIPort: 31417, NNodes: 1, RuntimeDriver: "docker",
					AssignedGPUs: tp * pp * dp, TensorParallel: tp, PipelineParallel: pp, Sleepable: true, Vendor: "ascend",
					TemplateArgs: spec.Args, TemplateEnv: spec.Env,
				},
			})
			if err != nil {
				t.Errorf("%s: sleepable launch refused: %v", where, err)
				continue
			}
			if strings.Contains(launch.Env["PYTORCH_NPU_ALLOC_CONF"], "expandable_segments:True") {
				t.Errorf("%s: PYTORCH_NPU_ALLOC_CONF=%q keeps expandable segments on", where, launch.Env["PYTORCH_NPU_ALLOC_CONF"])
			}
			warned := false
			for _, warning := range warnings {
				warned = warned || warning.Code == messages.WarningAscendSleepExpandableSegmentsDisabled
			}
			if warned != expandable {
				t.Errorf("%s: expandable segments rewrite warning = %v, want %v", where, warned, expandable)
			}
			additional, found := deploy.LaunchFlag(launch.Argv, "--additional-config")
			var config map[string]any
			if !found || json.Unmarshal([]byte(additional), &config) != nil || config["weight_nz_mode"] != float64(0) {
				t.Errorf("%s: --additional-config=%q, want weight_nz_mode 0", where, additional)
			}
		}
	}
	if checked == 0 || rewroteAllocator == 0 {
		t.Fatalf("checked %d templates, %d with expandable segments; the check would pass vacuously", checked, rewroteAllocator)
	}
}

// statedParallelism reads a parallelism degree a template states literally;
// an absent or placement-resolved value counts as 1.
func statedParallelism(args []string, flag string) int {
	value, _ := deploy.LaunchFlag(args, flag)
	degree, err := strconv.Atoi(value)
	if err != nil || degree < 1 {
		return 1
	}
	return degree
}
