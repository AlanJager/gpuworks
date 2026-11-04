package fleet

import (
	"fmt"
	"time"
)

const (
	// GPUReadTestUtilization and GPUReadTestMemoryBytes are the fleet-host's
	// deterministic telemetry values checked before the measured run.
	GPUReadTestUtilization = 37.0
	GPUReadTestMemoryBytes = float64(1 << 30)
)

// WaitGPUInventoryMetrics waits until every synthetic card appears with the
// deterministic samples emitted by fleet-host. This is a setup check, before
// the measured churn window, so an empty metrics store cannot look like a
// successful run of the GPU inventory hot path.
func WaitGPUInventoryMetrics(client *Client, expectedCards int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		var result struct {
			Items []struct {
				Load struct {
					UtilizationPct *float64 `json:"utilization_pct"`
					MemoryUsed     *float64 `json:"memory_used_bytes"`
				} `json:"load"`
			} `json:"items"`
		}
		if err := client.GetJSON("/api/v1/gpus", &result); err != nil {
			last = err.Error()
		} else {
			loaded := 0
			for _, item := range result.Items {
				if item.Load.UtilizationPct != nil && *item.Load.UtilizationPct == GPUReadTestUtilization &&
					item.Load.MemoryUsed != nil && *item.Load.MemoryUsed == GPUReadTestMemoryBytes {
					loaded++
				}
			}
			if len(result.Items) == expectedCards && loaded == expectedCards {
				return nil
			}
			last = fmt.Sprintf("inventory has %d cards, %d with expected fresh metrics; want %d", len(result.Items), loaded, expectedCards)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("GPU inventory metrics did not become ready within %s: %s", timeout, last)
}
