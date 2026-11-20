package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// CatalogPackageReferencesForGC returns the complete list of referenced
// catalog packages only after a fresh write has committed. A clustered store
// uses its strict local scheduling connection, so a follower cannot borrow
// the leader's authority through redirection, and business admission and the
// serving phase are checked in the same transaction. A reference that cannot
// be read fails the whole read: a shortened list must never look like
// permission to delete. This is a commit barrier, not a lease on later file
// deletion.
func (s *Store) CatalogPackageReferencesForGC(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db := s.db
	if s.schedulingDB != nil {
		db = s.schedulingDB
	}
	var references []string
	err := s.withBusinessWritePoolTx(ctx, db, func(tx *gorm.DB) error {
		result := tx.Exec("UPDATE catalog_gc_barriers SET sequence = sequence + 1 WHERE id = 1 AND typeof(sequence) = 'integer' AND sequence >= 0 AND sequence < ?", int64(math.MaxInt64))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("catalog garbage collection commit barrier unavailable")
		}
		var err error
		references, err = readCompleteCatalogPackageReferences(tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return references, nil
}

// readCompleteCatalogPackageReferences enumerates the references inside the
// caller's transaction.
func readCompleteCatalogPackageReferences(tx *gorm.DB) ([]string, error) {
	references := map[string]struct{}{}
	for _, source := range []struct {
		kind   string
		model  any
		column string
	}{
		{"inference model", &InferenceModel{}, "package_digest"},
		{"inference spec", &InferenceSpecRow{}, "package_digest"},
		{"deployment", &Deployment{}, "template_snapshot"},
		{"instance", &Instance{}, "spec"},
	} {
		var rows []struct {
			ID    uint
			Value string
		}
		if err := tx.Model(source.model).Select("id, " + source.column + " AS value").Where(source.column + " <> ''").Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("read %s package references: %w", source.kind, err)
		}
		for _, row := range rows {
			if err := addCompleteCatalogPackageReference(references, source.kind, row.ID, row.Value); err != nil {
				return nil, err
			}
		}
	}
	result := make([]string, 0, len(references))
	for digest := range references {
		result = append(result, digest)
	}
	slices.Sort(result)
	return result, nil
}

func addCompleteCatalogPackageReference(references map[string]struct{}, kind string, id uint, value string) error {
	var digests []string
	switch kind {
	case "deployment":
		// A null snapshot, omitted assets and null assets all mean no assets.
		var snapshot struct {
			RuntimeAssets []proto.RuntimeAsset `json:"runtime_assets"`
		}
		if json.Unmarshal([]byte(value), &snapshot) != nil {
			return fmt.Errorf("%s %d: invalid package reference JSON", kind, id)
		}
		for _, asset := range snapshot.RuntimeAssets {
			digests = append(digests, asset.PackageDigest)
		}
	case "instance":
		var desired proto.InstanceDesiredState
		if json.Unmarshal([]byte(value), &desired) != nil {
			return fmt.Errorf("%s %d: invalid package reference JSON", kind, id)
		}
		for _, asset := range desired.RuntimeAssets {
			digests = append(digests, asset.PackageDigest)
		}
	default:
		digests = []string{value}
	}
	for _, digest := range digests {
		if digest == "" {
			continue
		}
		digest = strings.ToLower(strings.TrimSpace(digest))
		if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
			return fmt.Errorf("%s %d: %w", kind, id, errInvalidCatalogPackageReference)
		}
		references[digest] = struct{}{}
	}
	return nil
}

var errInvalidCatalogPackageReference = errors.New("invalid catalog package digest")
