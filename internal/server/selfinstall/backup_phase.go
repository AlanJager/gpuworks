package selfinstall

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	sqlite "github.com/glebarez/go-sqlite"
	"gopkg.in/yaml.v3"
)

var ErrBackupRequiresDrain = errors.New("backup requires a drained maintenance phase")

// requireDrainedMaintenancePhase reads the on-disk phase without migrating it.
// A file that is not the replicated database keeps the existing snapshot path.
func requireDrainedMaintenancePhase(dataDir string) error {
	path := filepath.Join(dataDir, "gpuworks.db")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return nil
	}
	if err != nil {
		return err
	}
	locator := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", locator.String())
	if err != nil {
		return err
	}
	defer db.Close()
	var table string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'maintenance_phase'`).Scan(&table)
	if errors.Is(err, sql.ErrNoRows) || sqliteNotADatabase(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var phase string
	var revision int64
	err = db.QueryRow(`SELECT phase, revision FROM maintenance_phase WHERE id = 1`).Scan(&phase, &revision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: maintenance phase row is missing", ErrBackupRequiresDrain)
		}
		return err
	}
	if phase == "drained" {
		return nil
	}
	if phase == "serving" {
		single, err := ordinarySingleMemberDatabase(path)
		if err != nil {
			return err
		}
		if single {
			return nil
		}
	}
	return fmt.Errorf("%w: %s at revision %d", ErrBackupRequiresDrain, phase, revision)
}

// ordinarySingleMemberDatabase is true when the stopped database has no
// unfinished join and no second Raft member. The installer does not migrate
// the database to answer this.
func ordinarySingleMemberDatabase(dbPath string) (bool, error) {
	if _, err := os.Lstat(dbPath + ".join"); err == nil {
		return false, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	metadata := filepath.Join(dbPath+".dqlite", "cluster.yaml")
	info, err := os.Lstat(metadata)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
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
	return len(members) <= 1, nil
}

var ErrOlderSnapshotOverOpenBusiness = errors.New("older snapshot is not applied after business writes reopened")

// sqliteNotADatabase reports SQLite's SQLITE_NOTADB (26). glebarez exposes the
// code on its error and does not export the constant.
func sqliteNotADatabase(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 26
}
