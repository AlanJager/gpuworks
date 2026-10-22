package store

import (
	"sort"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// imageReferenceBatch bounds one IN list. SQLite allows 32766 parameters; a
// fixed, much smaller batch keeps every statement cheap and the plan stable.
const imageReferenceBatch = 500

// ImageReferenceCounts counts, per image ref, the deployments that name it,
// the instances not yet finished with it, and the models bound to its catalog
// row. Refs with no reference are absent from the map.
func (s *Store) ImageReferenceCounts(refs []string) (map[string]int, error) {
	return imageReferenceCounts(s.db, refs)
}

func imageReferenceCounts(db *gorm.DB, refs []string) (map[string]int, error) {
	out := map[string]int{}
	type refCount struct {
		Ref   string `gorm:"column:ref"`
		Count int    `gorm:"column:n"`
	}
	for start := 0; start < len(refs); start += imageReferenceBatch {
		batch := refs[start:min(start+imageReferenceBatch, len(refs))]
		for _, query := range []*gorm.DB{
			db.Table("deployments").Select("image AS ref, COUNT(*) AS n").Where("image IN ?", batch).Group("image"),
			db.Table("instances").Select("image AS ref, COUNT(*) AS n").
				Where("image IN ? AND state NOT IN ?", batch, proto.InstanceFinishedStates()).Group("image"),
			db.Table("models").Select("image_versions.image_ref AS ref, COUNT(*) AS n").
				Joins("JOIN image_versions ON image_versions.id = models.image_id").
				Where("image_versions.image_ref IN ?", batch).Group("image_versions.image_ref"),
		} {
			var rows []refCount
			if err := query.Scan(&rows).Error; err != nil {
				return nil, err
			}
			for _, row := range rows {
				out[row.Ref] += row.Count
			}
		}
	}
	return out, nil
}

// ReconcileMissingOfficialImages applies the catalog's removal rule to the
// system rows a manifest no longer lists (every official row whose ref is not
// in seen): a row nothing references is deleted together with its node
// presence rows and, when it was the last one, its series; a referenced row is
// deprecated so the deployments using it keep working. The writes are one
// transaction.
func (s *Store) ReconcileMissingOfficialImages(seen map[string]struct{}, since string) (deleted, deprecated int, err error) {
	// System rows are bounded by the manifest size, so reading them all and
	// taking the difference in memory beats a NOT IN over every seen ref.
	var official []Image
	if err := s.db.Select("id", "image_ref", "series_id", "deprecated", "deprecated_since").Where("source = ?", "official").Find(&official).Error; err != nil {
		return 0, 0, err
	}
	missing := make([]Image, 0, len(official))
	refs := make([]string, 0, len(official))
	for _, row := range official {
		if _, ok := seen[row.ImageRef]; ok {
			continue
		}
		missing = append(missing, row)
		refs = append(refs, row.ImageRef)
	}
	if len(missing) == 0 {
		return 0, 0, nil
	}
	counts, err := s.ImageReferenceCounts(refs)
	if err != nil {
		return 0, 0, err
	}
	var deprecateIDs, deleteIDs []uint
	var deprecatedRefs, deletedRefs []string
	seriesIDs := map[uint]struct{}{}
	for _, row := range missing {
		if counts[row.ImageRef] > 0 {
			// Already deprecated by an earlier catalog: keep the version that
			// first dropped it.
			if row.Deprecated && row.DeprecatedSince != "" {
				continue
			}
			deprecateIDs = append(deprecateIDs, row.ID)
			deprecatedRefs = append(deprecatedRefs, row.ImageRef)
			continue
		}
		deleteIDs = append(deleteIDs, row.ID)
		deletedRefs = append(deletedRefs, row.ImageRef)
		seriesIDs[row.SeriesID] = struct{}{}
	}
	affectedSeries := make([]uint, 0, len(seriesIDs))
	for id := range seriesIDs {
		affectedSeries = append(affectedSeries, id)
	}
	sort.Slice(affectedSeries, func(i, j int) bool { return affectedSeries[i] < affectedSeries[j] })
	var deletedSeries []string
	err = s.db.Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(deprecateIDs); start += imageReferenceBatch {
			batch := deprecateIDs[start:min(start+imageReferenceBatch, len(deprecateIDs))]
			if err := tx.Model(&Image{}).Where("id IN ?", batch).Updates(map[string]any{"deprecated": true, "deprecated_since": since}).Error; err != nil {
				return err
			}
		}
		for start := 0; start < len(deleteIDs); start += imageReferenceBatch {
			batch := deleteIDs[start:min(start+imageReferenceBatch, len(deleteIDs))]
			if err := tx.Where("image_version_id IN ?", batch).Delete(&ImageNode{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", batch).Delete(&Image{}).Error; err != nil {
				return err
			}
		}
		if len(affectedSeries) == 0 {
			return nil
		}
		empty := tx.Model(&ImageSeries{}).
			Where("source = ? AND id IN ?", "official", affectedSeries).
			Where("NOT EXISTS (SELECT 1 FROM image_versions WHERE image_versions.series_id = image_series.id)")
		if err := empty.Pluck("name", &deletedSeries).Error; err != nil {
			return err
		}
		if len(deletedSeries) == 0 {
			return nil
		}
		return tx.Where("source = ? AND name IN ?", "official", deletedSeries).Delete(&ImageSeries{}).Error
	})
	if err != nil {
		return 0, 0, err
	}
	for _, ref := range deletedRefs {
		s.Bus.PublishChange("image", ref, "deleted")
	}
	for _, name := range deletedSeries {
		s.Bus.PublishChange("image", name, "series_deleted")
	}
	for _, ref := range deprecatedRefs {
		s.Bus.PublishChange("image", ref, "updated")
	}
	return len(deleteIDs), len(deprecateIDs), nil
}
