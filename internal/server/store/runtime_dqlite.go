//go:build linux && dqlite

package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// ReplicatedBackend is the replicated database of a server built with the
// cluster runtime. A caller that wants it passes it; a runtime open without
// it is SQLite.
func ReplicatedBackend() RuntimeBackend {
	return RuntimeBackend{database: replicatedRuntimeDatabase{}}
}

// replicatedRuntimeDatabase is the Linux dqlite backend.
type replicatedRuntimeDatabase struct{}

// open opens the single-member runtime database. It never falls back to
// SQLite: an existing SQLite file is refused.
func (replicatedRuntimeDatabase) open(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error) {
	return openRuntimeDatabaseMode(ctx, path, b, existing, false)
}

// openWith is the single-member open with OpenWith's options.
// The policy is checked again once dqlite has created its directory.
func (d replicatedRuntimeDatabase) openWith(ctx context.Context, path string, b *bus.Bus, o OpenOptions) (*Store, error) {
	if err := checkRuntimeDirectoryPolicy(path, o.Policies...); err != nil {
		return nil, err
	}
	st, err := d.open(ctx, path, b, false)
	if err != nil {
		return nil, err
	}
	if err := st.initializeExtensionSchemas(ctx, o.Extensions); err != nil {
		return nil, errors.Join(err, st.Close())
	}
	if err := checkRuntimeDirectoryPolicy(path, o.Policies...); err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
}

func (replicatedRuntimeDatabase) openForMigration(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error) {
	if !existing {
		return nil, errors.New("offline migration requires an existing database")
	}
	return openRuntimeDatabaseMode(ctx, path, b, existing, true)
}

func openRuntimeDatabaseMode(ctx context.Context, path string, b *bus.Bus, existing, migration bool) (*Store, error) {
	if isInMemoryDSN(path) || strings.HasPrefix(path, "file:") {
		return nil, errors.New("dqlite runtime requires a database filesystem path, not a SQLite URI or in-memory DSN")
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("existing SQLite database %q requires explicit import and archival before starting dqlite; source left unchanged", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if info, err := os.Stat(path + ".dqlite"); err == nil {
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Geteuid()) {
			return nil, fmt.Errorf("run the native database as its directory owner; a different user could create unreadable persistence files in %s.dqlite", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if existing {
		if err := checkExistingNativeFiles(path + ".dqlite"); err != nil {
			return nil, err
		}
	}
	db, err := cluster.OpenConfiguredDatabase(ctx, path+".dqlite", filepath.Base(path))
	if err != nil {
		return nil, err
	}
	st, err := OpenReplicated(ctx, db.SQL, db.Scheduling, b, ReplicatedBackend())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	st.closeBackend = db.Close
	bindNativeRuntimeCallbacks(st, db)
	if clusterID, pin := db.TransportIdentity(); clusterID != "" {
		current, identityErr := st.ClusterIdentity(ctx)
		if identityErr != nil || current.ClusterID != clusterID || current.CAFingerprint != pin || current.Protocol != ClusterProtocolVersion {
			_ = st.Close()
			return nil, errors.Join(ErrClusterIdentityConflict, identityErr)
		}
		bindNativeClusterIdentity(st, db, ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: ClusterProtocolVersion}, ctx)
	}
	if existing {
		var err error
		if migration {
			err = st.migrateSchemaOffline(ctx)
		} else {
			err = st.CheckCurrentSchema(ctx)
		}
		if err != nil {
			_ = st.Close()
			if migration {
				return nil, fmt.Errorf("offline schema migration: %w", err)
			}
			return nil, fmt.Errorf("offline cluster initialization requires an existing current-schema database: %w", err)
		}
		return st, nil
	}
	if err := st.InitializeSchema(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}
	// This constructor only admits an established single member. Multi-member
	// terminal recovery must instead use effective session ownership.
	if err := retryStaleConnection(ctx, func() error {
		_, err := st.CloseOrphanTerminalSessions()
		return err
	}); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// staleConnectionRetryInterval separates attempts of an idempotent startup
// write whose pooled connection was dropped.
const staleConnectionRetryInterval = 200 * time.Millisecond

// retryStaleConnection repeats an idempotent write that failed only because
// its pooled connection was no longer usable; such a transaction did not
// commit. Any other error, the fifth failure and cancellation are returned.
func retryStaleConnection(ctx context.Context, write func() error) error {
	for attempt := 0; ; attempt++ {
		err := write()
		if err == nil || !errors.Is(err, driver.ErrBadConn) || attempt == 4 {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(staleConnectionRetryInterval):
		}
	}
}

func (replicatedRuntimeDatabase) openEstablishedCluster(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error) {
	return openExpandedRuntimeDatabase(ctx, path, b, existing, false)
}

func (replicatedRuntimeDatabase) openLegacyExpanded(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error) {
	return openExpandedRuntimeDatabase(ctx, path, b, existing, true)
}

func openExpandedRuntimeDatabase(ctx context.Context, path string, b *bus.Bus, existing, legacy bool) (*Store, error) {
	if !existing {
		return nil, errors.New("established cluster runtime requires existing native files")
	}
	if err := checkExistingNativeFiles(path + ".dqlite"); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("existing SQLite database %q requires explicit import", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if info, err := os.Stat(path + ".dqlite"); err != nil {
		return nil, err
	} else if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("run the native database as its directory owner: %s.dqlite", path)
	}
	db, err := cluster.OpenEstablishedMemberDatabase(ctx, path+".dqlite", filepath.Base(path))
	if err != nil {
		return nil, err
	}
	var st *Store
	if legacy {
		st, err = openOwnedLegacyExpandedMemberStore(ctx, db, b, db.Close)
	} else {
		st, err = OpenOwnedJoinedMemberStore(ctx, db, b, db.Close)
	}
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return st, nil
}

func bindNativeRuntimeCallbacks(st *Store, db *cluster.Database) {
	st.localSchedulingLeader = func(ctx context.Context) (bool, error) {
		leader, err := db.Leader(ctx)
		return err == nil && db.ID != 0 && leader.ID == db.ID, err
	}

}

func bindNativeClusterIdentity(st *Store, db *cluster.Database, expected ClusterIdentity, ctx context.Context) {
	st.bindTransportAuthorization(expected, db.TransportMemberKey(), db.ApplyTransportPolicySnapshot, db.TransportPolicySnapshot)
	st.bindClusterMembership(expected, db.Members)
	st.bindInferenceMembership(expected, db.ID, db.Address, db.TransportMemberKey())
	bindNativeActionLeader(st, db)
	if db.ID != 0 {
		st.NoteClusterSessionRoute(sessionRouteFromMembers(db.ID, db.Address, membersOrEmpty(ctx, db)))
	}
}

func bindNativeActionLeader(st *Store, db *cluster.Database) {
	st.currentClusterLeader = func(ctx context.Context) (uint64, error) {
		leader, err := db.Leader(ctx)
		if err != nil {
			return 0, err
		}
		if leader.Role != "voter" {
			return 0, errors.New("observed cluster leader is not a voter")
		}
		return leader.ID, nil
	}
}

// JoinedTransportAuthorizationRefresher binds a still-running join session to
// the replicated admission source. The Database owns both SQL pools and its
// transport policy lease; the returned closure only borrows them until the
// session stops. It must be called after the joined database is SQL-ready.
func JoinedTransportAuthorizationRefresher(ctx context.Context, db *cluster.Database) (func(context.Context) error, error) {
	if db == nil {
		return nil, errors.New("joined transport requires a running database")
	}
	clusterID, pin := db.TransportIdentity()
	key := db.TransportMemberKey()
	if !validClusterDigest(clusterID, 16) || !validClusterDigest(pin, 32) || !validClusterDigest(key, 32) {
		return nil, ErrClusterIdentityConflict
	}
	st, err := OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New(), ReplicatedBackend())
	if err != nil {
		return nil, err
	}
	expected := ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: ClusterProtocolVersion}
	current, err := st.ClusterIdentity(ctx)
	if err != nil || current.ClusterID != expected.ClusterID || current.CAFingerprint != expected.CAFingerprint || current.Protocol != expected.Protocol {
		return nil, errors.Join(ErrClusterIdentityConflict, err)
	}
	st.bindTransportAuthorization(expected, key, db.ApplyTransportPolicySnapshot, db.TransportPolicySnapshot)
	return st.RefreshTransportAuthorization, nil
}

// This check runs under the runtime lease, before dqlite can bootstrap an
// empty directory. Raft itself validates the contents when opening them.
func checkExistingNativeFiles(dir string) error {
	regular := func(name string) bool {
		info, err := os.Lstat(filepath.Join(dir, name))
		return err == nil && info.Mode().IsRegular() && info.Size() > 0
	}
	if !regular("info.yaml") || !regular("cluster.yaml") || (!regular("metadata1") && !regular("metadata2")) {
		return errors.New("existing native database identity and Raft metadata are required; restore missing files before cluster initialization")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		// Closed Raft segments are numeric ranges; snapshots retain the log
		// prefix in their filename. An open segment alone is not a cold DB.
		if (len(name) == 33 && name[16] == '-') || strings.HasPrefix(name, "snapshot-") {
			if regular(name) {
				return nil
			}
		}
	}
	return errors.New("existing native database has no persisted Raft log or snapshot; explicit recovery is required")
}
