// Package traffic counts the bytes this agent moves for a network role, so
// the collector can report them next to the NIC counters. It is process-wide
// state: every transfer of a role adds to the same counters.
package traffic

import "sync/atomic"

// RoleStorageSync is node-to-node model and image sync.
const RoleStorageSync = "storage_sync"

var storageSync struct{ rx, tx atomic.Uint64 }

// AddStorageSyncRx counts bytes received from a peer node.
func AddStorageSyncRx(n int) {
	if n > 0 {
		storageSync.rx.Add(uint64(n))
	}
}

// AddStorageSyncTx counts bytes sent to a peer node.
func AddStorageSyncTx(n int) {
	if n > 0 {
		storageSync.tx.Add(uint64(n))
	}
}

// Totals is the cumulative rx and tx bytes per role since the agent started.
func Totals() map[string][2]uint64 {
	return map[string][2]uint64{RoleStorageSync: {storageSync.rx.Load(), storageSync.tx.Load()}}
}
