//go:build linux && dqlite

package app

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// removalObservation binds the caller's observation to one call. A nil observation
// stays nil, and the removal rules refuse it.
func removalObservation(ctx context.Context, online func(context.Context, cluster.Member) bool) func(cluster.Member) bool {
	if online == nil {
		return nil
	}
	return func(member cluster.Member) bool { return online(ctx, member) }
}

// RemoveClusterMember drops a live member only when three members remain and
// at least two of the remaining voters answer; see memberRemovalRefusal for
// what force gives up. online reports whether a member answers now. It does
// not open the application or create a cluster.
func RemoveClusterMember(ctx context.Context, db *cluster.Database, id uint64, online func(context.Context, cluster.Member) bool, force bool) error {
	if db == nil {
		return cluster.ErrUnavailable
	}
	members, err := db.Members(ctx)
	if err != nil {
		return err
	}
	if err := memberRemovalRefusal(members, removalObservation(ctx, online), id, force); err != nil {
		return err
	}
	return db.Remove(ctx, id)
}

// ReplaceClusterMember removes oldID only after newID is already a voter. It
// promotes a joined spare when the current voter set can accept that
// promotion, and refuses before promoting when the removal that follows would
// be refused. It does not open the application.
func ReplaceClusterMember(ctx context.Context, db *cluster.Database, oldID, newID uint64, online func(context.Context, cluster.Member) bool, force bool) error {
	if db == nil {
		return cluster.ErrUnavailable
	}
	members, err := db.Members(ctx)
	if err != nil {
		return err
	}
	promote, err := memberReplacementRefusal(members, removalObservation(ctx, online), oldID, newID, force)
	if err != nil {
		return err
	}
	if promote {
		if err := db.Assign(ctx, newID, "voter"); err != nil {
			return err
		}
	}
	return db.Remove(ctx, oldID)
}
