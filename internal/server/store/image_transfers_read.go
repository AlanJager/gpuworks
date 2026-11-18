package store

// ActiveImageTransfers reads current preparation facts on the WAL reader.
// Finished, failed and cancelled transfers do not keep a deployment waiting.
func (s *Store) ActiveImageTransfers() ([]ImageNode, error) {
	var rows []ImageNode
	err := s.reader().Where("state IN ?", []string{ImageStateSyncing, ImageStatePulling}).Find(&rows).Error
	return rows, err
}
