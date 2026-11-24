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
//
// Between this and a node start the later one decides what
// ProcessStorageTier reports. A server does each once and in this order: it
// is told its tier when the command starts and then starts its one node,
// which records what it was started with; nothing tells the process a tier
// after that. The other order belongs to processes that start more than one
// node (a command that opens the database twice, a test binary): there the
// tier told last replaces what an earlier node ran, until the next node
// start records its own.
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
