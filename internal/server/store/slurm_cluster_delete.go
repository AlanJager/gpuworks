package store

import (
	"gpuworks.ai/gpuworks/internal/proto"
)

// SlurmDeleteBlockers is what keeps an entry from being deleted together
// with its nodes: agents of it that are connected, and nodes of it that hold
// instances (a row only waiting to be removed does not count: the node's
// deletion takes it along).
type SlurmDeleteBlockers struct {
	Online    int
	Instances int
}

// SlurmClusterDeleteBlockers counts, for each of the given entries (every
// entry with nodes when none is given), what blocks deleting it with its
// nodes. Two grouped queries however many entries and nodes there are, both
// on the unique node index's `slurm_cluster_id > 0` predicate; an entry with
// no blocker is absent from the answer.
func (s *Store) SlurmClusterDeleteBlockers(clusterIDs ...uint) (map[uint]SlurmDeleteBlockers, error) {
	out := map[uint]SlurmDeleteBlockers{}
	type count struct {
		SlurmClusterID uint
		N              int
	}
	var online, held []count
	q := s.reader().Table("nodes").Select("slurm_cluster_id, count(*) AS n").
		Where("slurm_cluster_id > 0 AND state = ?", NodeStateOnline).Group("slurm_cluster_id")
	if len(clusterIDs) > 0 {
		q = q.Where("slurm_cluster_id IN ?", clusterIDs)
	}
	if err := q.Scan(&online).Error; err != nil {
		return nil, err
	}
	q = s.reader().Table("nodes").Select("nodes.slurm_cluster_id AS slurm_cluster_id, count(DISTINCT nodes.id) AS n").
		Joins("JOIN instances ON instances.node_id = nodes.id AND instances.state NOT IN ?", []string{proto.InstRemoving, proto.InstPendingRemoval}).
		Where("nodes.slurm_cluster_id > 0").Group("nodes.slurm_cluster_id")
	if len(clusterIDs) > 0 {
		q = q.Where("nodes.slurm_cluster_id IN ?", clusterIDs)
	}
	if err := q.Scan(&held).Error; err != nil {
		return nil, err
	}
	for _, r := range online {
		b := out[r.SlurmClusterID]
		b.Online = r.N
		out[r.SlurmClusterID] = b
	}
	for _, r := range held {
		b := out[r.SlurmClusterID]
		b.Instances = r.N
		out[r.SlurmClusterID] = b
	}
	return out, nil
}

// SlurmClusterNodes lists the nodes of an entry.
func (s *Store) SlurmClusterNodes(clusterID uint) ([]Node, error) {
	var nodes []Node
	err := s.reader().Where("slurm_cluster_id > 0 AND slurm_cluster_id = ?", clusterID).Order("id").Find(&nodes).Error
	return nodes, err
}
