//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// OpenJoinedMemberStore borrows a running Join session's two SQL pools and
// binds its actual Raft membership to authorization reads. The session keeps
// ownership of the pools; callers must close the session after all Store use.
func OpenJoinedMemberStore(ctx context.Context, db *cluster.Database, b *bus.Bus) (*Store, error) {
	if db == nil || db.ID == 0 {
		return nil, errors.New("joined member store requires a running member")
	}
	clusterID, pin := db.TransportIdentity()
	if clusterID == "" || pin == "" {
		return nil, ErrClusterIdentityConflict
	}
	st, err := OpenReplicated(ctx, db.SQL, db.Scheduling, b, ReplicatedBackend())
	if err != nil {
		return nil, err
	}
	expected := ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: ClusterProtocolVersion}
	if err := st.CheckClusterCompatibility(ctx, expected); err != nil {
		return nil, err
	}
	bindNativeRuntimeCallbacks(st, db)
	bindNativeClusterIdentity(st, db, expected, ctx)
	return st, nil
}

// OpenOwnedJoinedMemberStore transfers a live joined member to a runtime Store.
// The owner callback must stop the Raft session and release its runtime lease.
// On failure ownership stays with the caller; on success Store.Close invokes
// the callback after closing SQL pools. Opening never initializes schema,
// recovers terminal work, removes join markers, or grants business readiness.
func OpenOwnedJoinedMemberStore(ctx context.Context, db *cluster.Database, b *bus.Bus, closeOwner func() error) (*Store, error) {
	return openOwnedMemberStore(ctx, db, b, closeOwner, false)
}

func openOwnedLegacyExpandedMemberStore(ctx context.Context, db *cluster.Database, b *bus.Bus, closeOwner func() error) (*Store, error) {
	return openOwnedMemberStore(ctx, db, b, closeOwner, true)
}

func openOwnedMemberStore(ctx context.Context, db *cluster.Database, b *bus.Bus, closeOwner func() error, legacy bool) (*Store, error) {
	if db == nil || db.ID == 0 || db.SQL == nil || db.Scheduling == nil || closeOwner == nil {
		return nil, errors.New("owned joined member store requires a running member and close owner")
	}
	clusterID, pin := db.TransportIdentity()
	key := db.TransportMemberKey()
	if !validClusterDigest(clusterID, 16) || !validClusterDigest(pin, 32) || !validClusterDigest(key, 32) {
		return nil, ErrClusterIdentityConflict
	}
	st, err := OpenReplicated(ctx, db.SQL, db.Scheduling, b, ReplicatedBackend())
	if err != nil {
		return nil, err
	}
	expected := ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: ClusterProtocolVersion}
	var compatibilityErr error
	if legacy {
		compatibilityErr = st.checkLegacyExpandedCompatibility(ctx, expected)
	} else {
		compatibilityErr = st.CheckClusterCompatibility(ctx, expected)
	}
	if compatibilityErr != nil {
		return nil, compatibilityErr
	}
	bindOwnedMemberStore(ctx, st, db, expected, closeOwner)
	return st, nil
}

func bindOwnedMemberStore(ctx context.Context, st *Store, db *cluster.Database, expected ClusterIdentity, closeOwner func() error) {
	bindNativeRuntimeCallbacks(st, db)
	bindNativeClusterIdentity(st, db, expected, ctx)
	var once sync.Once
	var closeErr error
	st.closeBackend = func() error {
		once.Do(func() { closeErr = closeOwner() })
		return closeErr
	}
}
