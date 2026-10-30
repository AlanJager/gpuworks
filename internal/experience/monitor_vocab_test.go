package experience

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/platformmetrics"
)

// TestBackfilledSeriesArePlatformMetrics keeps the history the environment
// writes in the platform's own vocabulary (platformmetrics, and
// inferencemetrics for the engine and gateway families): a renamed metric
// would leave the demo's charts a month short, silently, and a counter
// written as a gauge would chart nonsense rates.
func TestBackfilledSeriesArePlatformMetrics(t *testing.T) {
	kinds := map[string]inferencemetrics.Kind{}
	for _, d := range platformmetrics.Definitions() {
		kinds[d.Series] = inferencemetrics.Kind(d.Kind)
	}
	for _, d := range inferencemetrics.Definitions() {
		kinds[d.Series()] = d.Kind
	}
	for _, d := range inferencemetrics.GatewayDefinitions() {
		kinds[d.Series()] = d.Kind
	}
	for name, shape := range backfilled {
		family := name
		for _, suffix := range []string{"_sum", "_count"} {
			if trimmed, ok := strings.CutSuffix(name, suffix); ok && kinds[trimmed] == inferencemetrics.Histogram {
				family = trimmed
			}
		}
		kind, ok := kinds[family]
		if !ok {
			t.Errorf("backfilled %s is no platform metric", name)
			continue
		}
		if kind == inferencemetrics.Counter && !shape.counter {
			t.Errorf("backfilled %s is a counter but grows as a gauge", name)
		}
		if kind == inferencemetrics.Gauge && shape.counter {
			t.Errorf("backfilled %s is a gauge but grows as a counter", name)
		}
	}
}
