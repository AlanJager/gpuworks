package deploy

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// runveilThreadLimit is the per-process size the CPU thread pools are capped
// to under runveil.
const runveilThreadLimit = "4"

// runveilThreadEnv names the thread pools that are capped. The set is the one
// measured together on a real node, not a minimal one: no variable was tested
// alone, so none is dropped on theory (docs/explanation/runveil-and-udocker.md,
// 线程很多的工作负载).
var runveilThreadEnv = []string{
	"OMP_NUM_THREADS",
	"MKL_NUM_THREADS",
	"OPENBLAS_NUM_THREADS",
	"NUMEXPR_NUM_THREADS",
	"TORCH_NUM_THREADS",
}

// runveilEnv is the system-layer environment an engine needs on a node whose
// runtime is runveil (driver "udocker"). PRoot traces every thread from one
// tracer, and SGLang and vLLM ranks size CPU-side pools from the host's
// cores. An 8-GPU SGLang rank set ran about 3600 threads and starved the
// tracer until weight loading stalled; an 8-GPU vLLM rank set on a 240-core
// host ran 157 threads per worker, its ranks initialized one after another,
// and capping the pools started it about two minutes sooner with the same
// throughput (docs/explanation/runveil-and-udocker.md, 线程很多的工作负载).
//
// It is server-side so the preview shows it and a deployment can remove any
// key with env_unset; the template and the deployment's own env still
// override every key.
func runveilEnv(engine string, p Params) map[string]string {
	if (engine != EngineSGLang && engine != EngineVLLM) ||
		!strings.EqualFold(strings.TrimSpace(p.RuntimeDriver), proto.RuntimePreferenceUdocker) {
		return nil
	}
	env := make(map[string]string, len(runveilThreadEnv))
	for _, key := range runveilThreadEnv {
		env[key] = runveilThreadLimit
	}
	return env
}
