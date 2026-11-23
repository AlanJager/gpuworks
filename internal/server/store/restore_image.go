package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gorm.io/gorm"
)

// ErrRestoreImageSchema reports a database image this executable cannot
// restore: its migration markers are not a leading part of the executable's
// own list.
var ErrRestoreImageSchema = errors.New("database image has a schema this executable does not know")

// ImageMigrationVersions returns the migration markers of a database image,
// in order. It reads the file and changes nothing.
func ImageMigrationVersions(ctx context.Context, imagePath string) ([]int, error) {
	source, err := openRestoreImage(ctx, imagePath)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	return imageMigrationVersions(ctx, source)
}

// ImageExtensionMigrationVersions returns the applied steps of every
// extension namespace in a database image. An image no extension has been
// opened with has none.
func ImageExtensionMigrationVersions(ctx context.Context, imagePath string) (map[string][]int, error) {
	source, err := openRestoreImage(ctx, imagePath)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	var table string
	err = source.QueryRowContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table' AND name = 'namespace_migrations'").Scan(&table)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := source.QueryContext(ctx, "SELECT namespace, version FROM namespace_migrations ORDER BY namespace, version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := make(map[string][]int)
	for rows.Next() {
		var namespace string
		var version int
		if err := rows.Scan(&namespace, &version); err != nil {
			return nil, err
		}
		versions[namespace] = append(versions[namespace], version)
	}
	return versions, rows.Err()
}

func openRestoreImage(ctx context.Context, imagePath string) (*sql.DB, error) {
	imagePath, err := filepath.Abs(imagePath)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(imagePath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("database image must be a regular file")
	}
	// The image is a closed file of a backup: nothing writes it, and it has
	// no write-ahead log beside it although its header names that mode.
	return openSQLiteImportReader(ctx, imagePath, true)
}

func imageMigrationVersions(ctx context.Context, source *sql.DB) ([]int, error) {
	rows, err := source.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("%w: read migration markers: %w", ErrRestoreImageSchema, err)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

// validateRestoreImageMigrations accepts the markers of this executable's
// list up to any point: an image of an earlier release is restored as it is,
// on its own schema, and upgraded afterwards like the cluster it came from.
// Markers this executable does not know, and a list with a gap, are refused.
func validateRestoreImageMigrations(ctx context.Context, source *sql.DB) error {
	applied, err := imageMigrationVersions(ctx, source)
	if err != nil {
		return err
	}
	known := make([]int, 0, len(migrations))
	for _, m := range migrations {
		known = append(known, m.Version)
	}
	sort.Ints(known)
	if len(applied) == 0 || len(applied) > len(known) {
		return fmt.Errorf("%w: %d markers, this executable knows %d", ErrRestoreImageSchema, len(applied), len(known))
	}
	for i, version := range applied {
		if version != known[i] {
			return fmt.Errorf("%w: marker %d where this executable expects %d", ErrRestoreImageSchema, version, known[i])
		}
	}
	return nil
}

// RestoreDatabaseImage loads a database image into this store's empty
// database in one transaction: every table, row, index and sequence of the
// image, and nothing else. The image keeps its own schema; no migration runs
// here, so an image of an earlier release yields a database that release
// opens. The caller keeps the target out of service and supplies the
// encryption material the image was written with.
func (s *Store) RestoreDatabaseImage(ctx context.Context, imagePath string) error {
	if s == nil || s.db == nil {
		return errors.New("a database image is restored into an open store")
	}
	source, err := openRestoreImage(ctx, imagePath)
	if err != nil {
		return fmt.Errorf("database image: %w", err)
	}
	defer source.Close()
	schema, err := inspectSQLiteSource(ctx, source, validateRestoreImageMigrations)
	if err != nil {
		return fmt.Errorf("database image: %w", err)
	}
	target := s.db
	if s.schedulingDB != nil {
		target = s.schedulingDB
	}
	// The target is empty: it has no admission table to read until the image
	// is in it. The caller keeps it out of service.
	if err := importSQLiteSnapshot(withoutWriterGuard(ctx), target, source, schema); err != nil {
		return fmt.Errorf("restore database image: %w", err)
	}
	return nil
}

// RevokeOtherClusterAdmissions revokes every admission except key's, for a
// database restored onto one member: the other members of the image are not
// members of the restored cluster until they join it again. It returns the
// addresses revoked now, and every revoked key the database then holds, in
// order: the restored member's first transport policy has to name them, since
// a running member never adds a revocation to its own policy. key must hold
// an admission that is not revoked.
func (s *Store) RevokeOtherClusterAdmissions(ctx context.Context, clusterID, key string) (addresses, revokedKeys []string, err error) {
	if !validClusterDigest(key, 32) {
		return nil, nil, ErrClusterAdmissionRejected
	}
	err = s.clusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		addresses, revokedKeys = nil, nil
		if err := requireClusterIdentity(tx, clusterID); err != nil {
			return err
		}
		var admissions []ClusterAdmission
		if err := tx.Order("key_fingerprint").Find(&admissions).Error; err != nil {
			return err
		}
		kept := false
		now := tx.NowFunc().UTC()
		for _, admission := range admissions {
			switch {
			case admission.RevokedAt != nil:
				revokedKeys = append(revokedKeys, admission.KeyFingerprint)
				continue
			case admission.KeyFingerprint == key:
				kept = true
				continue
			}
			result := tx.Model(&ClusterAdmission{}).Where("key_fingerprint = ? AND revoked_at IS NULL", admission.KeyFingerprint).UpdateColumn("revoked_at", now)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrClusterAdmissionRejected
			}
			addresses = append(addresses, admission.Address)
			revokedKeys = append(revokedKeys, admission.KeyFingerprint)
		}
		if !kept {
			return fmt.Errorf("%w: the restoring member holds no admission in the image", ErrClusterAdmissionRejected)
		}
		if len(addresses) == 0 {
			return nil
		}
		return advanceClusterAuthorization(tx)
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(addresses)
	return addresses, revokedKeys, nil
}
