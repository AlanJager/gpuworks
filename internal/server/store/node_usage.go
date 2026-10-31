package store

import "gpuworks.ai/gpuworks/internal/proto"

// usageModelFileStates are the model file states whose downloaded bytes sit
// on a node's disk: finished, registered, and the partial bytes of a
// download, sync or failed attempt that were not cleaned up yet.
var usageModelFileStates = []string{proto.MFDownloading, proto.MFSyncing, proto.MFFailed, proto.MFRegistered, proto.MFPresent}

// NodeDiskUsage is the control-plane managed bytes on each node, by node id:
// model file bytes in usageModelFileStates and present image bytes. The
// storage usage page reads it once for the whole fleet instead of two
// queries per node.
func (s *Store) NodeDiskUsage() (models, images map[uint]int64, err error) {
	type row struct {
		NodeID uint
		Bytes  int64
	}
	var modelRows, imageRows []row
	if err = s.reader().Model(&ModelFile{}).Select("node_id, SUM(downloaded) AS bytes").
		Where("node_id <> 0 AND state IN ?", usageModelFileStates).Group("node_id").Scan(&modelRows).Error; err != nil {
		return nil, nil, err
	}
	if err = s.reader().Model(&ImageNode{}).Select("node_id, SUM(size_bytes) AS bytes").
		Where("state = ?", ImageStatePresent).Group("node_id").Scan(&imageRows).Error; err != nil {
		return nil, nil, err
	}
	models = make(map[uint]int64, len(modelRows))
	for _, r := range modelRows {
		models[r.NodeID] = r.Bytes
	}
	images = make(map[uint]int64, len(imageRows))
	for _, r := range imageRows {
		images[r.NodeID] = r.Bytes
	}
	return models, images, nil
}
