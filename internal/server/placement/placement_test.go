package placement

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const scalableResolver = `
def resolve(gpu):
    if gpu.count < 4:
        return reject("至少需要 4 张 GPU")
    if gpu.count % 4 != 0:
        return reject("GPU 数量必须是 4 的倍数")
    if gpu.min_memory_gib < 32:
        return reject("单卡显存至少需要 32 GiB")
    return {"GPU_COUNT": gpu.count, "TP_SIZE": gpu.count, "SP_DEGREE": 1}
`

func TestScalableResolverAcceptsFourAndEightButRejectsTwo(t *testing.T) {
	if err := Compile(scalableResolver); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		count      int
		wantReject bool
	}{
		{count: 2, wantReject: true}, {count: 4}, {count: 8},
	} {
		result, err := Resolve(context.Background(), scalableResolver, GPU{Count: test.count, Vendor: "ascend", MinMemoryGiB: 32})
		if err != nil {
			t.Fatal(err)
		}
		if (result.Reason != "") != test.wantReject {
			t.Fatalf("count=%d result=%+v", test.count, result)
		}
		if !test.wantReject && result.Variables["TP_SIZE"] != int64(test.count) {
			t.Fatalf("count=%d variables=%+v", test.count, result.Variables)
		}
	}
}

func TestResolverAcceptDeclaresMeasuredMemoryAndLaunchVariables(t *testing.T) {
	source := `
def resolve(gpu):
    if gpu.count != 4:
        return reject("该档位只验证过 4 卡")
    return accept(
        gpu_memory_mib_per_device = 29424,
        variables = {"TP_SIZE": gpu.count, "SP_DEGREE": 1},
    )
`
	if err := Compile(source); err != nil {
		t.Fatal(err)
	}
	rejected, err := Resolve(context.Background(), source, GPU{Count: 2, MinMemoryGiB: 32})
	if err != nil || rejected.Reason == "" {
		t.Fatalf("two-card result=%+v err=%v", rejected, err)
	}
	accepted, err := Resolve(context.Background(), source, GPU{Count: 4, MinMemoryGiB: 32})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Reason != "" || accepted.GPUMemoryMiBPerDevice != 29424 || accepted.Variables["TP_SIZE"] != int64(4) || accepted.Variables["SP_DEGREE"] != int64(1) {
		t.Fatalf("accepted result=%+v", accepted)
	}
}

func TestResolverAcceptRejectsInvalidResourceContracts(t *testing.T) {
	for _, source := range []string{
		"def resolve(gpu):\n    return accept(gpu_memory_mib_per_device=0)\n",
		fmt.Sprintf("def resolve(gpu):\n    return accept(gpu_memory_mib_per_device=%d)\n", MaxGPUMemoryMiBPerDevice+1),
		"def resolve(gpu):\n    return accept(gpu_memory_mib_per_device=1024, variables={\"BAD\": [gpu.count]})\n",
	} {
		if _, err := Resolve(context.Background(), source, GPU{Count: 4}); err == nil {
			t.Fatalf("invalid resource contract was accepted: %s", source)
		}
	}
}

func TestResolverRejectsImportsLoopsAndCompositeOutputs(t *testing.T) {
	tests := []struct{ source, want string }{
		{"load(\"host.star\", \"secret\")\ndef resolve(gpu): return {}\n", "load/import is disabled"},
		{"def resolve(gpu):\n    for _ in range(1000000000):\n        pass\n", "too many steps"},
		{"def resolve(gpu):\n    return {\"BAD\": [gpu.count]}\n", "forbidden list"},
	}
	for _, test := range tests {
		_, err := Resolve(context.Background(), test.source, GPU{Count: 4})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("source=%q error=%v, want %q", test.source, err, test.want)
		}
	}
}

func TestResolverRejectsSingleStepMemoryAmplification(t *testing.T) {
	for _, source := range []string{
		"def resolve(gpu):\n    return {\"BAD\": \"x\" * 1000000000}\n",
		"def resolve(gpu):\n    return {\"BAD\": [x for x in range(100000)]}\n",
		"def resolve(gpu):\n    return {\"BAD\": 1 << 1000000000}\n",
		"def resolve(gpu):\n    values = []\n    values.append(gpu.count)\n    return {\"BAD\": 1}\n",
	} {
		if _, err := Resolve(context.Background(), source, GPU{Count: 4}); err == nil {
			t.Fatalf("memory-amplifying resolver was accepted: %s", source)
		}
	}
}

func TestResolverBoundsInputsAndScalarOutput(t *testing.T) {
	tooMany := GPU{Count: MaxGPUs + 1}
	if _, err := Resolve(context.Background(), scalableResolver, tooMany); err == nil {
		t.Fatal("oversized GPU input was accepted")
	}
	tooManyVariables := "def resolve(gpu):\n    return {" + strings.Join(func() []string {
		values := make([]string, MaxResultVariables+1)
		for index := range values {
			values[index] = fmt.Sprintf("\"V_%d\": %d", index, index)
		}
		return values
	}(), ",") + "}\n"
	if _, err := Resolve(context.Background(), tooManyVariables, GPU{Count: 4}); err == nil {
		t.Fatal("oversized resolver output was accepted")
	}
}

func TestExpandPreservesGenericPreviewAndRequiresConcreteVariables(t *testing.T) {
	values := []string{"--tp-size", "${TP_SIZE}", "prefix-${SP_DEGREE}"}
	got, err := Expand(values, map[string]any{"TP_SIZE": int64(8)}, true)
	if err != nil || got[1] != "8" || got[2] != "prefix-${SP_DEGREE}" {
		t.Fatalf("generic=%v err=%v", got, err)
	}
	if _, err := Expand(values, map[string]any{"TP_SIZE": int64(8)}, false); err == nil {
		t.Fatal("concrete expansion accepted an unresolved variable")
	}
}

// TestRuntimeVariablesLeavePipelineParallelToItsOwner pins the ownership rule
// for ${PP_SIZE}. The engine owns the default pipeline degree (the node count
// for a multi-node launch, no flag at all for a single node), so placement must
// neither invent one nor hide a spec that forgot to return it: an unresolved
// placeholder has to stop the launch, not quietly pick a degree.
func TestRuntimeVariablesLeavePipelineParallelToItsOwner(t *testing.T) {
	unpinned := RuntimeVariables(Result{Variables: map[string]any{}}, 4, 4, "/model")
	if _, present := unpinned["PP_SIZE"]; present {
		t.Fatalf("placement supplied PP_SIZE=%v; the engine owns that default", unpinned["PP_SIZE"])
	}
	if _, err := Expand([]string{"--pipeline-parallel-size", "${PP_SIZE}"}, unpinned, false); err == nil {
		t.Fatal("a spec that references ${PP_SIZE} without returning it still expanded")
	}
	pinned := RuntimeVariables(Result{Variables: map[string]any{"TP_SIZE": int64(1), "PP_SIZE": int64(4)}}, 4, 4, "/model")
	values, err := Expand([]string{"--tensor-parallel-size", "${TP_SIZE}", "--pipeline-parallel-size", "${PP_SIZE}"}, pinned, false)
	if err != nil || values[1] != "1" || values[3] != "4" {
		t.Fatalf("expanded a pinned PP4 profile=%v err=%v", values, err)
	}
}

// A resolver runs before a node, a port or a rank exists. The names only the
// launch can state are refused from it, through both result shapes, so a
// guessed value can never shadow the fact; GPU_COUNT and TP_SIZE stay
// returnable as they always were.
func TestResolverCannotReturnLaunchOwnedVariables(t *testing.T) {
	for _, name := range []string{VarModelPath, VarServedModelName, VarAPIHost, VarAPIPort, VarNNodes, VarNodeRank, VarHeadless, VarNodeAddr, VarMasterAddr, VarMasterPort} {
		for _, body := range []string{
			fmt.Sprintf(`return {%q: 1}`, name),
			fmt.Sprintf(`return accept(gpu_memory_mib_per_device=1024, variables={%q: 1})`, name),
		} {
			source := "def resolve(gpu):\n    " + body + "\n"
			if _, err := Resolve(context.Background(), source, GPU{Count: 1}); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s: Resolve() = %v, want a refusal naming %s", body, err, name)
			}
		}
	}
	result, err := Resolve(context.Background(), "def resolve(gpu):\n    return {\"GPU_COUNT\": gpu.count, \"TP_SIZE\": 1}\n", GPU{Count: 2})
	if err != nil || result.Variables[VarTPSize] != int64(1) {
		t.Fatalf("GPU_COUNT/TP_SIZE result=%v err=%v", result, err)
	}
}
