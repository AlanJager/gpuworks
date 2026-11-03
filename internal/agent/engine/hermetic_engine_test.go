package engine

import (
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"log/slog"
)

// newHermeticEngine makes general engine tests independent of the host GPU.
// Vendor-specific tests use NewWithProber with their own explicit prober.
func newHermeticEngine(driver runtime.Driver, report Reporter, log *slog.Logger, stateDir string) *Engine {
	return NewWithProber(driver, report, log, stateDir, injectionProber{})
}
