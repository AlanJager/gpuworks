//go:build !linux || !dqlite

package store

// ReplicatedReadInterrupted is always false without the replicated database:
// a local SQLite read has no leadership transfer to retry across.
func ReplicatedReadInterrupted(error) bool { return false }

// ReplicatedLeaderUnavailable is always false without the replicated
// database: a local SQLite connection has no leader to elect.
func ReplicatedLeaderUnavailable(error) bool { return false }

// ReplicatedWriterBusy is always false without the replicated database: the
// local SQLite pool queues its writers itself.
func ReplicatedWriterBusy(error) bool { return false }
