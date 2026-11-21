package store

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// RuntimeBackend is the database a runtime Store runs on. The zero value is
// SQLite, the single-node database of the community server. A server built
// with the cluster runtime passes the replicated backend instead; nothing is
// selected at compile time and nothing is registered.
type RuntimeBackend struct {
	// database is nil for SQLite.
	database runtimeDatabase
}

// runtimeDatabase is what differs between the backends: how each kind of
// runtime open reaches its database under the runtime lease, and which of
// its errors mean only that a leader could not be reached.
type runtimeDatabase interface {
	// open is the single-member open, or with existing the open for offline
	// metadata operations.
	open(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error)
	// openWith is the single-member open with OpenWith's options.
	openWith(ctx context.Context, path string, b *bus.Bus, o OpenOptions) (*Store, error)
	// openForMigration opens a stopped single-member database and applies
	// only schema migrations.
	openForMigration(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error)
	// openEstablishedCluster reopens a multi-member database without startup
	// writes.
	openEstablishedCluster(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error)
	// openLegacyExpanded reopens a multi-member database of the schema
	// before coordinated migration.
	openLegacyExpanded(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error)
	leaderUnavailable(err error) bool
	readInterrupted(err error) bool
}

func (b RuntimeBackend) runtimeDatabase() runtimeDatabase {
	if b.database == nil {
		return sqliteRuntimeDatabase{}
	}
	return b.database
}

// ReplicatedLeaderUnavailable reports a dial that found no elected leader.
// It is always false on SQLite: a local connection has no leader to elect.
func (b RuntimeBackend) ReplicatedLeaderUnavailable(err error) bool {
	return b.runtimeDatabase().leaderUnavailable(err)
}

// ReplicatedReadInterrupted reports a failure response of the replicated
// database that interrupted a read during a leadership transfer; callers may
// retry only side-effect-free reads on it. It is always false on SQLite: a
// local read has no leadership transfer to retry across.
func (b RuntimeBackend) ReplicatedReadInterrupted(err error) bool {
	return b.runtimeDatabase().readInterrupted(err)
}

// RuntimeBackend is the backend whose connections this Store holds, so a
// caller with one of its errors can classify it, or reopen the same database
// after the Store is closed. A Store that Open or OpenWith returned is on
// SQLite.
func (s *Store) RuntimeBackend() RuntimeBackend {
	if s == nil {
		return RuntimeBackend{}
	}
	return s.backend
}
