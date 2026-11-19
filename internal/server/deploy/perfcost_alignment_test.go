package deploy

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
)

// The deploy form's performance switches and the engine performance registry
// (engines.ScanPerformanceCosts) describe the same flags from two sides: what
// a quick parameter renders, and what that rendering costs. Rendering each
// switch through the real dictionary must produce a launch the registry grades
// as that cost, so neither side can rename a flag alone.
func TestQuickPerformanceSwitchesRenderRegisteredCosts(t *testing.T) {
	for _, tc := range []struct {
		engine string
		raw    map[string]any
		want   engines.PerformanceCost
	}{
		{EngineVLLM, map[string]any{"enforce_eager": true}, engines.CostGraphDisabled},
		{EngineSGLang, map[string]any{"enforce_eager": true}, engines.CostGraphDisabled},
		{EngineVLLM, map[string]any{"enable_prefix_caching": false}, engines.CostPrefixCacheDisabled},
		{EngineSGLang, map[string]any{"enable_prefix_caching": false}, engines.CostPrefixCacheDisabled},
		{EngineVLLM, map[string]any{"enable_chunked_prefill": false}, engines.CostChunkedPrefillDisabled},
		{EngineVLLM, map[string]any{"max_num_seqs": 2}, engines.CostConcurrencyCapped},
		{EngineSGLang, map[string]any{"max_num_seqs": 2}, engines.CostConcurrencyCapped},
	} {
		materialized, err := Materialize(MaterializeInput{Engine: tc.engine, Raw: tc.raw})
		if err != nil {
			t.Fatalf("%s %v: %v", tc.engine, tc.raw, err)
		}
		engine, err := SelectEngine(tc.engine)
		if err != nil {
			t.Fatal(err)
		}
		built := engine.Build(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1, Quick: materialized.Quick})
		var costs []engines.PerformanceCost
		for _, finding := range engines.ScanPerformanceCosts(tc.engine, built.Argv, nil) {
			costs = append(costs, finding.Cost)
			if finding.Cost == tc.want && finding.Level != engines.LevelGuarded {
				t.Errorf("%s %v renders %s at level %s, want guarded", tc.engine, tc.raw, finding.Param, finding.Level)
			}
		}
		if len(costs) != 1 || costs[0] != tc.want {
			t.Errorf("%s %v renders %q graded as %v, want only %s", tc.engine, tc.raw, built.Argv, costs, tc.want)
		}
	}
	// The switches' defaults cost nothing.
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		selected, err := SelectEngine(engine)
		if err != nil {
			t.Fatal(err)
		}
		materialized, err := Materialize(MaterializeInput{Engine: engine})
		if err != nil {
			t.Fatal(err)
		}
		built := selected.Build(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1, Quick: materialized.Quick})
		if found := engines.ScanPerformanceCosts(engine, built.Argv, nil); len(found) != 0 {
			t.Errorf("%s default launch %q graded as %+v", engine, built.Argv, found)
		}
	}
}
