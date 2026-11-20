//go:build linux && dqlite

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
)

type importRecord struct {
	Format               int       `json:"format"`
	Reason               string    `json:"reason"`
	StartedAt            time.Time `json:"started_at"`
	Phase                string    `json:"phase"`
	Database             string    `json:"database"`
	OperationDir         string    `json:"operation_dir"`
	ExternalMasterSecret bool      `json:"external_master_secret"`
}

// ImportRuntimeSQLite converts a stopped, current-schema installation in place. All
// non-database material stays in the original data directory. The returned
// directory contains a consistent database backup and the archived source.
// The caller must stop old binaries too: they do not honor the runtime lock.
func ImportRuntimeSQLite(ctx context.Context, dataDir, dbPath string) (result string, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		return "", err
	}
	owner, ok := dataInfo.Sys().(*syscall.Stat_t)
	if !dataInfo.IsDir() || !ok {
		return "", errors.New("SQLite import requires the original data directory")
	}
	if owner.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("run SQLite import as the original data directory owner (uid %d); importing as uid %d would leave the service unable to open its native database", owner.Uid, os.Geteuid())
	}
	if err := CheckRestoreCompleted(dataDir); err != nil {
		return "", err
	}
	dbPath, release, err := LockRuntimeDatabase(dbPath)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	if err := CheckRestoreCompleted(filepath.Dir(dbPath)); err != nil {
		return "", err
	}
	for _, path := range []string{dbPath + ".maintenance", dbPath + ".dqlite"} {
		if _, err := os.Lstat(path); err == nil {
			return "", fmt.Errorf("import refuses existing %s; inspect and recover the prior operation first", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	info, err := os.Lstat(dbPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("SQLite import source must be a regular file")
	}
	// Never generate replacement encryption material. An environment override
	// remains an external dependency, as it is during ordinary service startup.
	externalKey := os.Getenv("GPUWORKS_MASTER_SECRET") != ""
	keyPath := filepath.Join(dataDir, "master.key")
	key, err := os.ReadFile(keyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if len(key) == 0 && !externalKey {
		return "", errors.New("original master.key or GPUWORKS_MASTER_SECRET is required; import will not generate a replacement key")
	}
	operationDir, err := os.MkdirTemp(filepath.Dir(dbPath), ".gpuworks-sqlite-import-")
	if err != nil {
		return "", err
	}
	record := importRecord{Format: 1, Reason: "sqlite-to-dqlite", StartedAt: time.Now().UTC(), Phase: "preparing", Database: dbPath, OperationDir: operationDir, ExternalMasterSecret: externalKey}
	if len(key) != 0 {
		if err := writeNewSynced(filepath.Join(operationDir, "master.key"), key); err != nil {
			return operationDir, err
		}
	}
	if err := writeRecord(operationDir, record); err != nil {
		return operationDir, err
	}
	marker := dbPath + ".maintenance"
	markerData, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return operationDir, err
	}
	if err := writeNewSynced(marker, markerData); err != nil {
		return operationDir, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("SQLite import incomplete; preserve %s and %s, follow its recovery.txt before starting: %w", marker, operationDir, resultErr)
		}
	}()
	recovery := "This is a same-machine offline import, not a full-system backup. Keep all services stopped.\n" +
		"Inspect manifest.json and the maintenance marker before changing files. Never delete the backup directory.\n" +
		"If the original database path still exists and no original.db is archived here, the source has not been activated: retain the prepared target and remove the maintenance marker only after confirming the original SQLite is intact.\n" +
		"If original.db exists here, restore it and any original.db-wal/original.db-shm to the original database path only after isolating any published .dqlite directory. Do not merge native directories or overwrite an existing source.\n" +
		"backup.db is the consistent SQLite snapshot if original sidecars are incomplete. Restore it as a whole database with no stale WAL/SHM.\n" +
		"Keep the original data directory and effective master secret. A native server refuses a restored SQLite file; use the matching SQLite server to recover.\n" +
		"Only remove the maintenance marker after a complete selected state is restored. Never start a service while both source and target are missing.\n"
	if err := writeNewSynced(filepath.Join(operationDir, "recovery.txt"), []byte(recovery)); err != nil {
		return operationDir, err
	}
	staged := filepath.Join(operationDir, "native")
	db, err := cluster.OpenSingleDatabase(ctx, staged, filepath.Base(dbPath))
	if err != nil {
		return operationDir, err
	}
	target, err := OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New())
	if err != nil {
		return operationDir, errors.Join(err, db.Close())
	}
	importErr := target.ImportSQLite(ctx, dbPath, filepath.Join(operationDir, "backup.db"))
	if importErr == nil {
		secret := string(key)
		if externalKey {
			secret = os.Getenv("GPUWORKS_MASTER_SECRET")
		}
		importErr = target.validateImportedSecrets(ctx, secret)
	}
	if importErr == nil {
		importErr = target.InitializeSchema(ctx)
	}
	closeErr := errors.Join(target.Close(), db.Close())
	if err := errors.Join(importErr, closeErr); err != nil {
		return operationDir, err
	}
	if err := ctx.Err(); err != nil {
		return operationDir, err
	}
	record.Phase = "prepared"
	if err := writeRecord(operationDir, record); err != nil {
		return operationDir, err
	}
	// Persist the closed native files before publishing the directory. dqlite
	// itself owns their layout; never synthesize or merge Raft files.
	if err := syncTree(staged); err != nil {
		return operationDir, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		source := dbPath + suffix
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		} else if err != nil {
			return operationDir, err
		}
		if err := os.Rename(source, filepath.Join(operationDir, "original.db"+suffix)); err != nil {
			return operationDir, err
		}
	}
	if err := syncDir(operationDir); err != nil {
		return operationDir, err
	}
	if err := syncDir(filepath.Dir(dbPath)); err != nil {
		return operationDir, err
	}
	if err := os.Rename(staged, dbPath+".dqlite"); err != nil {
		return operationDir, err
	}
	if err := syncDir(operationDir); err != nil {
		return operationDir, err
	}
	if err := syncDir(filepath.Dir(dbPath)); err != nil {
		return operationDir, err
	}
	record.Phase = "activated"
	if err := writeRecord(operationDir, record); err != nil {
		return operationDir, err
	}
	if err := os.Remove(marker); err != nil {
		return operationDir, err
	}
	if err := syncDir(filepath.Dir(dbPath)); err != nil {
		return operationDir, err
	}
	return operationDir, nil
}

func writeRecord(dir string, record importRecord) error {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	temp := filepath.Join(dir, "manifest.next")
	if err := writeNewSynced(temp, raw); err != nil {
		return err
	}
	if err := os.Rename(temp, filepath.Join(dir, "manifest.json")); err != nil {
		return err
	}
	return syncDir(dir)
}

func writeNewSynced(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func syncTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected native database file type: %s", path)
		}
		return syncDir(path)
	})
}
