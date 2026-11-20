package store

import (
	"context"
	"time"
)

// DeploymentIDsForReconcile discovers durable targets without loading their
// potentially large launch snapshots. Failed scans never return partial work.
func (s *Store) DeploymentIDsForReconcile(ctx context.Context) ([]uint, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ids []uint
	if err := s.reader().WithContext(ctx).Model(&Deployment{}).Order("id ASC").Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
