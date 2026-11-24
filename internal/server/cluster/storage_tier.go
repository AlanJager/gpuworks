package cluster

import (
	"sync/atomic"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// processStorageTier is the tier of the one installation this process serves.
// Most paths that start the database node know only its directory, so the
// command that owns the process records the tier here once, before any start.
var processStorageTier atomic.Pointer[storagetier.Tier]

// UseStorageTier sets the tier every database node started by this process
// runs with, unless its DatabaseConfig names one. A command calls it once,
// with the tier its installation records, before it opens the database.
func UseStorageTier(tier storagetier.Tier) {
	processStorageTier.Store(&tier)
}

// ProcessStorageTier is the tier this process runs its database node with:
// the one it was told at its start, which is what the member runs until it
// is restarted, whatever its installation records by now.
func ProcessStorageTier() storagetier.Tier {
	return DatabaseConfig{}.storageTier()
}

// storageTier resolves the tier a node starts with: the configuration's own,
// then the process's, then standard.
func (c DatabaseConfig) storageTier() storagetier.Tier {
	if c.StorageTier != "" {
		return c.StorageTier
	}
	if tier := processStorageTier.Load(); tier != nil && *tier != "" {
		return *tier
	}
	return storagetier.Standard
}
