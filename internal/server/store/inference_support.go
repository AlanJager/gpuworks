package store

// ListDistinctOnlineDetectedGPUs is the bounded chip-model summary used by
// automatic-placement recommendations; it does not load the fleet's nodes.
func (s *Store) ListDistinctOnlineDetectedGPUs() ([]DetectedGPU, error) {
	var rows []DetectedGPU
	err := s.db.Table("gpus").
		Select("lower(trim(gpus.vendor)) AS vendor, trim(gpus.name) AS name, trim(gpus.compute_capability) AS cc, trim(gpus.arch_id) AS arch_id").
		Joins("JOIN nodes ON nodes.id = gpus.node_id").
		Where("nodes.state = ? AND trim(gpus.name) <> ''", "online").
		Group("lower(trim(gpus.vendor)), trim(gpus.name), trim(gpus.compute_capability), trim(gpus.arch_id)").
		Order("vendor, name, cc, arch_id").Scan(&rows).Error
	return rows, err
}
