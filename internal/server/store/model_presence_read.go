package store

// ModelPresenceFacts reads only the requested models' file counters and the
// identities needed to count their locations. Empty slices in files preserve
// known models without copies; missing/deleted model IDs are omitted. No
// model metadata or per-node reads are needed as the visible page grows.
func (s *Store) ModelPresenceFacts(ids []uint) (files map[uint][]ModelFile, storages map[uint]*Storage, nodes map[uint]*Node, err error) {
	files, storages, nodes = map[uint][]ModelFile{}, map[uint]*Storage{}, map[uint]*Node{}
	if len(ids) == 0 {
		return
	}
	var models []Model
	if err = s.reader().Select("id").Where("id IN ?", ids).Find(&models).Error; err != nil {
		return
	}
	known := make([]uint, 0, len(models))
	for _, m := range models {
		files[m.ID] = nil
		known = append(known, m.ID)
	}
	if len(known) == 0 {
		return
	}
	var rows []ModelFile
	if err = s.reader().Where("model_id IN ?", known).Find(&rows).Error; err != nil {
		return
	}
	storageIDs, nodeIDs := map[uint]bool{}, map[uint]bool{}
	for _, file := range rows {
		files[file.ModelID] = append(files[file.ModelID], file)
		storageIDs[file.StorageID] = true
		if file.NodeID != 0 {
			nodeIDs[file.NodeID] = true
		}
	}
	if len(storageIDs) > 0 {
		ids := make([]uint, 0, len(storageIDs))
		for id := range storageIDs {
			ids = append(ids, id)
		}
		var rows []Storage
		if err = s.reader().Select("id", "type").Where("id IN ?", ids).Find(&rows).Error; err != nil {
			return
		}
		for i := range rows {
			storages[rows[i].ID] = &rows[i]
		}
	}
	if len(nodeIDs) > 0 {
		ids := make([]uint, 0, len(nodeIDs))
		for id := range nodeIDs {
			ids = append(ids, id)
		}
		var rows []Node
		if err = s.reader().Select("id").Where("id IN ?", ids).Find(&rows).Error; err != nil {
			return
		}
		for i := range rows {
			nodes[rows[i].ID] = &rows[i]
		}
	}
	return
}
