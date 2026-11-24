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
// with the tier its installation records, before it opens the database. What
// an earlier node of this process was started with no longer counts: the
// process's tier is the one it was told last, until it starts a node again.
func UseStorageTier(tier storagetier.Tier) {
	processStorageTier.Store(&tier)
	runningStorageTier.Store(nil)
}

// runningStorageTier is the tier the database node this process started was
// given: its configuration's own when it names one (a joining member's),
// else the process's.
var runningStorageTier atomic.Pointer[storagetier.Tier]

// ProcessStorageTier is the tier this process runs its database node with,
// whatever its installation records by now: the tier its node was started
// with, and before a node is started the one the process was told. It is the
// one source of the member's timing for everything outside the node: the
// membership grant's retry window, how long a leader observation is acted
// on, the liveness limit of the connections this process dials.
func ProcessStorageTier() storagetier.Tier {
	if tier := runningStorageTier.Load(); tier != nil {
		return *tier
	}
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
