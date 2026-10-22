package cloud

import "context"

func (s *Store) HasProviderNodes(ctx context.Context, providerID uint) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Table("nodes").Where("id IN (?) OR uuid IN (?)",
		s.db.Table("provider_instances").Select("node_id").Where("provider_id = ?", providerID),
		s.db.Model(&CloudHost{}).Select("node_uuid").Where("provider_id = ?", providerID),
	).Count(&count).Error
	return count > 0, err
}

func (s *Store) HasUnreleasedProviderLeases(ctx context.Context, providerID uint) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&CloudResourceLease{}).
		Where("provider_id = ? AND state <> ?", providerID, CloudLeaseReleased).
		Count(&count).Error
	return count > 0, err
}
