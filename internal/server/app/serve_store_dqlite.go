//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// serveStoreOpenTimeout bounds starting the single-member database and
// applying its schema.
const serveStoreOpenTimeout = 2 * time.Minute

// closedMember is what New has established for a member whose business is
// closed, kept by the runtime that will later open it: once the cluster
// admits the member, the runtime adopts the extension schemas on its store
// and builds each business generation with assembleBusiness.
type closedMember struct {
	cfg    Config
	log    *slog.Logger
	events *bus.Bus
	policy securefile.Policy
}

// memberDatabaseState reads, before anything opens the database, which
// lifecycle the files on disk belong to. A join marker or a recorded
// multi-member Raft membership is a member whose business starts closed; it
// must never reach the single-member open, which would bootstrap a
// standalone database beside the cluster's.
func memberDatabaseState(dbPath string) (joining, expanded bool, err error) {
	if _, err := os.Lstat(dbPath + ".join"); err == nil {
		return true, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, false, err
	}
	expanded, err = store.HasExpandedRuntime(dbPath)
	if err != nil {
		return false, false, fmt.Errorf("classify runtime database: %w", err)
	}
	return false, expanded, nil
}

// openClosedMember returns the App of a joining or established member, whose
// business is closed until the cluster admits it, and nil for a single
// member.
func openClosedMember(cfg Config, events *bus.Bus, log *slog.Logger) (*App, error) {
	if err := store.CheckRestoreCompleted(cfg.DataDir); err != nil {
		return nil, err
	}
	joining, expanded, err := memberDatabaseState(cfg.DBPath)
	if err != nil || (!joining && !expanded) {
		return nil, err
	}
	if filepath.Clean(cfg.DBPath) != filepath.Clean(filepath.Join(cfg.DataDir, "gpuworks.db")) {
		return nil, errors.New("a cluster member requires the database path inside its data directory")
	}
	// The same credential policy guards a member before it has a store. Its
	// credential key is the cluster's, read with the enrollment materials;
	// a member never generates one.
	policy, err := protectedCredentialPolicy(cfg, log)
	if err != nil {
		return nil, err
	}
	member := closedMember{cfg: cfg, log: log, events: events, policy: policy}
	if joining {
		runtime, err := newJoiningMemberRuntime(member)
		if err != nil {
			return nil, err
		}
		return newWaitingJoinedApp(runtime)
	}
	runtime, err := newEstablishedMemberRuntime(member)
	if err != nil {
		return nil, err
	}
	return newClosedSeedApp(runtime)
}

// openServeStore opens the single-member replicated store under the same
// credential policy and account steps as the SQLite store. A member
// initialized for enrollment uses the key its materials are bound to and
// fails without it; only a server that was never a cluster generates one.
func openServeStore(cfg Config, events *bus.Bus, log *slog.Logger) (*store.Store, string, haRuntime, error) {
	policy, err := protectedCredentialPolicy(cfg, log)
	if err != nil {
		return nil, "", nil, err
	}
	bundle, err := readEnrollmentBundle(cfg.DataDir, false)
	if err != nil {
		return nil, "", nil, err
	}
	master, err := enrollmentAppSecret(cfg, bundle, policy)
	if err != nil {
		return nil, "", nil, fmt.Errorf("load credential encryption key: %w", err)
	}
	// openClosedMember ran first; the store rechecks the join marker under
	// its runtime lease.
	ctx, cancel := context.WithTimeout(context.Background(), serveStoreOpenTimeout)
	options := storeOptions(cfg, policy)
	options.Backend = store.ReplicatedBackend()
	st, err := store.OpenRuntimeWith(ctx, cfg.DBPath, events, options)
	cancel()
	if err != nil {
		return nil, "", nil, err
	}
	// Bound before any worker or scoped copy shares the store.
	st.SetLogger(log)
	if err := admitCredentialStore(cfg, st, log, policy); err != nil {
		// The refusal is returned as it is, so a caller can still read its
		// product code; a failed close is added only when there is one.
		if closeErr := st.Close(); closeErr != nil {
			return nil, "", nil, errors.Join(err, closeErr)
		}
		return nil, "", nil, err
	}
	ha, err := newSingleMemberRuntime(cfg, bundle)
	if err != nil {
		return nil, "", nil, errors.Join(err, st.Close())
	}
	return st, master, ha, nil
}
