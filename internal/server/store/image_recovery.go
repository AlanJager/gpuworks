package store

// DirectImageTransfersForNode returns only pending peer transfers whose source
// or target is reconnecting. An unrelated node must not scan the image catalog
// or queue one query per image behind the control-plane writer.
func (s *Store) DirectImageTransfersForNode(nodeID uint) ([]ImageNode, error) {
	var rows []ImageNode
	err := s.reader().Where("state = ? AND source_node_id <> 0 AND (node_id = ? OR source_node_id = ?)", ImageStateSyncing, nodeID, nodeID).Find(&rows).Error
	for i := range rows {
		rows[i].ImageID = rows[i].ImageVersionID
	}
	return rows, err
}
