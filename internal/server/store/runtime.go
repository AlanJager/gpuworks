package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/filelock"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// OpenRuntime holds exclusive process ownership until Store.Close. Offline
// maintenance uses the same lock; legacy binaries do not honor this protocol.
// The zero backend is SQLite.
func OpenRuntime(ctx context.Context, path string, b *bus.Bus, backend RuntimeBackend) (*Store, error) {
	return openRuntimeWith(ctx, path, b, false, backend.runtimeDatabase().open)
}

// OpenRuntimeForMigration opens an existing stopped single-member database
// under the runtime lease and applies only schema migrations. Expanded Raft
// clusters need a separate coordinated quorum migration operation.
func OpenRuntimeForMigration(ctx context.Context, path string, b *bus.Bus, backend RuntimeBackend) (*Store, error) {
	return openRuntimeWith(ctx, path, b, true, backend.runtimeDatabase().openForMigration)
}

// OpenExistingRuntime opens an established native database for offline metadata
// operations. It neither initializes schema nor runs application recovery.
func OpenExistingRuntime(ctx context.Context, path string, b *bus.Bus, backend RuntimeBackend) (*Store, error) {
	return openRuntimeWith(ctx, path, b, true, backend.runtimeDatabase().open)
}

// HasExpandedRuntime inspects the durable Raft membership before the ordinary
// runtime can perform startup writes. A damaged membership is an error, not a
// reason to try single-member startup. The opener rechecks under its lease.
func HasExpandedRuntime(path string) (bool, error) {
	// An in-memory database has no Raft directory, so it is a single-member
	// process. A missing path or a file DSN still cannot be classified.
	if path == ":memory:" {
		return false, nil
	}
	if path == "" || strings.HasPrefix(path, "file:") {
		return false, errors.New("expanded runtime requires a filesystem database path")
	}
	metadata := filepath.Join(path+".dqlite", "cluster.yaml")
	info, err := os.Lstat(metadata)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 128<<10 {
		return false, errors.New("native membership file must be regular and bounded")
	}
	raw, err := os.ReadFile(metadata)
	if err != nil {
		return false, err
	}
	var members []struct {
		ID      uint64 `yaml:"ID"`
		Address string `yaml:"Address"`
	}
	if err := yaml.Unmarshal(raw, &members); err != nil {
		return false, fmt.Errorf("read native membership: %w", err)
	}
	return len(members) > 1, nil
}

// OpenExpandedRuntime reopens an existing multi-member database
// without performing application startup writes or granting business readiness.
func OpenExpandedRuntime(ctx context.Context, path string, b *bus.Bus, backend RuntimeBackend) (*Store, error) {
	return openRuntimeWith(ctx, path, b, true, backend.runtimeDatabase().openEstablishedCluster)
}

// OpenLegacyExpandedRuntime reopens an established v114 multi-member database
// under its runtime lease without schema migration or business writes. The
// returned Store is capable of writes; callers must keep it in a closed
// control-plane lifecycle until coordinated migration is complete. It must
// never be passed to ordinary application startup.
func OpenLegacyExpandedRuntime(ctx context.Context, path string, b *bus.Bus, backend RuntimeBackend) (*Store, error) {
	return openRuntimeWith(ctx, path, b, true, backend.runtimeDatabase().openLegacyExpanded)
}

func openRuntimeWith(ctx context.Context, path string, b *bus.Bus, existing bool, open func(context.Context, string, *bus.Bus, bool) (*Store, error)) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" || path == ":memory:" {
		if existing {
			return nil, errors.New("existing runtime requires a filesystem database path")
		}
		return open(ctx, path, b, false)
	}
	path, release, err := LockRuntimeDatabase(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path + ".join"); err == nil {
		return nil, errors.Join(errors.New("cluster join is incomplete; retain its member identity and resume joining instead of starting a standalone database"), release())
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(err, release())
	}
	if _, err := os.Lstat(path + ".maintenance"); err == nil {
		return nil, errors.Join(fmt.Errorf("database maintenance is incomplete; inspect %s.maintenance and recover before starting", path), release())
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(err, release())
	}
	if err := CheckRestoreCompleted(filepath.Dir(path)); err != nil {
		return nil, errors.Join(err, release())
	}
	st, err := open(ctx, path, b, existing)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	closeBackend := st.closeBackend
	st.closeBackend = func() error {
		var err error
		if closeBackend != nil {
			err = closeBackend()
		}
		return errors.Join(err, release())
	}
	return st, nil
}

// CheckRestoreCompleted prevents opening mixed generations after an interrupted
// restore. Callers with a custom database path must also check their data root.
func CheckRestoreCompleted(dataDir string) error {
	marker := filepath.Join(dataDir, ".restore-in-progress")
	if _, err := os.Lstat(marker); err == nil {
		return fmt.Errorf("data restoration is incomplete; inspect %s before starting or modifying the database", marker)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// LockRuntimeDatabase returns a canonical path and an exclusive maintenance
// lease. Callers must close all database handles before releasing it. The lock
// file is permanent: removing it could allow two owners on different inodes.
func LockRuntimeDatabase(path string) (string, func() error, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return "", nil, errors.New("runtime database requires a filesystem path, not a SQLite URI or in-memory DSN")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0750); err != nil {
		return "", nil, err
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return "", nil, err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	for _, candidate := range []string{abs, abs + ".dqlite", abs + ".runtime.lock"} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("runtime database path must not be a symbolic link: %s", candidate)
		}
	}
	release, err := filelock.Acquire(abs + ".runtime.lock")
	if err != nil {
		return "", nil, fmt.Errorf("lock runtime database %s: %w", abs, err)
	}
	return abs, release, nil
}
