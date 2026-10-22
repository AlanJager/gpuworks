package seed

import (
	"context"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

const (
	deepinferV41Model          = "deepseek-ai/deepseek-v4.1-flash-1017"
	deepinferSingleNodeSpec    = "fp4-deepinfer-nvidia-blackwell-ep"
	deepinferMultiNodeSpec     = "fp4-deepinfer-nvidia-blackwell-ep-multinode"
	deepinferSingleNodeShmByte = int64(450) << 30
)

// Every family the DeepInfer specs map is one the image's server exposes -
// checked against its real /metrics output (internal/inferencemetrics/testdata/
// deepinfer-serve.metrics) - and the declaration resolves to an observation
// that maps each of them.
func TestDeepInferMetricsMapRealExpositionFamilies(t *testing.T) {
	raw, err := os.ReadFile("../../../inferencemetrics/testdata/deepinfer-serve.metrics")
	if err != nil {
		t.Fatal(err)
	}
	exposed := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if end := strings.IndexAny(line, "{ "); end >= 0 {
			name = line[:end]
		}
		exposed[name] = true
	}
	for _, name := range []string{deepinferSingleNodeSpec, deepinferMultiNodeSpec} {
		_, spec := ppuSpec(t, deepinferV41Model, name)
		if spec.API == nil || spec.API.Metrics == nil || len(spec.API.Metrics.Map) == 0 {
			t.Fatalf("%s declares no metrics", name)
		}
		for source, target := range spec.API.Metrics.Map {
			definition, ok := inferencemetrics.Lookup(target.To)
			if !ok {
				t.Fatalf("%s maps %s to unknown %s", name, source, target.To)
			}
			family := source
			if definition.Kind == inferencemetrics.Histogram {
				family += "_bucket"
			}
			if !exposed[family] {
				t.Errorf("%s maps %s, which the DeepInfer server does not expose", name, source)
			}
		}
		observation := deploy.ResolveObservation(spec.Engine, spec.API.Metrics)
		if len(observation.Instance.MetricRules) != len(spec.API.Metrics.Map) || len(observation.Instance.MetricDetect) != 0 {
			t.Fatalf("%s resolves to %+v", name, observation.Instance)
		}
	}
}

// DeepInfer runs expert parallel over every GPU it is given, so both specs take
// 4 or 8 GPUs per node and prefer the larger count; the single-node spec keeps
// its Engram tables in /dev/shm, the multi-node one reads them over RDMA and
// only references the rendezvous address a multi-node launch provides.
func TestDeepInferSpecsPlaceOnFourOrEightBlackwellGPUs(t *testing.T) {
	for _, name := range []string{deepinferSingleNodeSpec, deepinferMultiNodeSpec} {
		_, spec := ppuSpec(t, deepinferV41Model, name)
		for count, wantAccept := range map[int]bool{2: false, 4: true, 8: true, 16: false} {
			memories, names := make([]float64, count), make([]string, count)
			for index := range memories {
				memories[index], names[index] = 179, "NVIDIA B200"
			}
			result, err := placement.Resolve(context.Background(), spec.Placement.Resolver,
				placement.GPU{Count: count, Vendor: "nvidia", Names: names, MemoriesGiB: memories, MinMemoryGiB: 179})
			if err != nil {
				t.Fatalf("%s count %d: %v", name, count, err)
			}
			if accepted := result.Reason == ""; accepted != wantAccept {
				t.Fatalf("%s count %d accepted=%v, want %v (%s)", name, count, accepted, wantAccept, result.Reason)
			}
			if wantAccept && result.Prefer != int64(count) {
				t.Fatalf("%s count %d prefers %d, want the count so a whole node wins", name, count, result.Prefer)
			}
		}
		references := placement.References(strings.Join(spec.Args, " "))
		multi := false
		for _, variable := range references {
			multi = multi || variable == placement.VarMasterAddr
		}
		if multi != (name == deepinferMultiNodeSpec) {
			t.Fatalf("%s references MASTER_ADDR=%v", name, multi)
		}
	}
	_, single := ppuSpec(t, deepinferV41Model, deepinferSingleNodeSpec)
	if single.ShmBytes != deepinferSingleNodeShmByte {
		t.Fatalf("single-node shm_bytes = %d, want %d", single.ShmBytes, deepinferSingleNodeShmByte)
	}
	// The single-node spec pins its Engram tables with cudaHostRegister, so its
	// container needs an unlimited memlock; a multi-node launch gets one from
	// the host-RDMA network it requires.
	_, multi := ppuSpec(t, deepinferV41Model, deepinferMultiNodeSpec)
	if !single.MemlockUnlimited || multi.MemlockUnlimited {
		t.Fatalf("memlock_unlimited single=%v multi=%v, want true/false", single.MemlockUnlimited, multi.MemlockUnlimited)
	}
	if single.Env["DEEPINFER_ENGRAM_LOCAL_FETCH"] != "1" || single.Env["EP_DISABLE_GIN"] != "1" {
		t.Fatalf("single-node spec must select the local Engram fetch: %v", single.Env)
	}
}
