//go:build linux && dqlite

package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// CompleteCatalogPackageReferences reads every catalog package a cluster
// member must hold from one bounded database snapshot. Unlike
// ReferencedCatalogPackageDigests it fails on a reference it cannot read: a
// member that copies or proves packages from a shortened list would report
// itself complete while missing one.
func (s *Store) CompleteCatalogPackageReferences(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var rows []struct {
		Source int
		ID     uint
		Value  string
	}
	// One statement keeps a single read snapshot and stays out of the writer
	// admission queue.
	const query = `SELECT 0 AS source, id, package_digest AS value FROM inference_models WHERE package_digest <> ''
		UNION ALL SELECT 1, id, package_digest FROM inference_specs WHERE package_digest <> ''
		UNION ALL SELECT 2, id, template_snapshot FROM deployments WHERE template_snapshot <> ''
		UNION ALL SELECT 3, id, spec FROM instances WHERE spec <> ''`
	if err := s.db.WithContext(ctx).Raw(query).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("read catalog package reference inventory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	references := map[string]struct{}{}
	for source, kind := range []string{"inference model", "inference spec", "deployment", "instance"} {
		for _, row := range rows {
			if row.Source != source {
				continue
			}
			if err := addCompleteCatalogPackageReference(references, kind, row.ID, row.Value); err != nil {
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
