package store

// WaitingCacheOperations returns only unfinished operation keys. A nil node
// selection is reserved for the startup recovery sweep; an empty selection
// does no work. Event handlers pass the affected nodes explicitly.
func (s *Store) WaitingCacheOperations(nodeIDs []uint) ([]StorageCacheOperation, error) {
	if nodeIDs != nil && len(nodeIDs) == 0 {
		return nil, nil
	}
	q := s.db.Select("storage_id", "node_id", "state").Where("state IN ?", []string{"pending", "waiting"})
	if nodeIDs != nil {
		q = q.Where("node_id IN ?", sortedUnique(nodeIDs))
	}
	var rows []StorageCacheOperation
	err := q.Find(&rows).Error
	return rows, err
}
