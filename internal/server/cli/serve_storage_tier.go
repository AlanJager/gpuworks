package cli

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// useRecordedStorageTier makes every database node this command starts run
// the storage tier the installation in dataDir records. A command that opens
// the replicated database calls it first; an installation that records no
// tier, or is not a cluster member, runs the standard one.
func useRecordedStorageTier(dataDir string) error {
	tier, err := app.RecordedStorageTier(dataDir)
	if err != nil {
		return fmt.Errorf("storage tier of %s: %w", dataDir, err)
	}
	cluster.UseStorageTier(tier)
	return nil
}
