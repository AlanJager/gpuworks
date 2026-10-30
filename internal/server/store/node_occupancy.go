package store

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
)

// AwakeGPUHolders is, for every node, the GPU indexes an awake instance holds:
// one read over the instance table, lean columns only, for ranking a whole
// candidate pool at once. A sleeping instance keeps only its residue and is
// left out; so are instances that hold nothing any more (stopped, failed). It
// is an approximation for ordering - which candidate to try first - and never
// decides a placement: the scheduler's own ledger does that per node.
func (s *Store) AwakeGPUHolders() (map[uint]map[int]bool, error) {
	// Columns are named by field: GORM spells GPUIndexes' column
	// "gp_uindexes", and a hand-written name is how a query drifts from the
	// entity.
	var rows []Instance
	err := s.reader().Select("NodeID", "GPUIndexes").
		Where("state NOT IN ? AND sleep_level = 0", []string{proto.InstStopped, proto.InstFailed}).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := map[uint]map[int]bool{}
	for _, r := range rows {
		var indexes []int
		if json.Unmarshal([]byte(r.GPUIndexes), &indexes) != nil {
			continue
		}
		if out[r.NodeID] == nil {
			out[r.NodeID] = map[int]bool{}
		}
		for _, index := range indexes {
			out[r.NodeID][index] = true
		}
	}
	return out, nil
}

// SleepingGPUHolders are the instances asleep on their cards (a reported
// sleep level, still holding a claim), lean: node, deployment and cards. A
// sleeper is charged its residue unless a placement keeps its whole claim
// for a request's wake (planning.HeldGPUs).
func (s *Store) SleepingGPUHolders() ([]Instance, error) {
	var rows []Instance
	err := s.reader().Select("NodeID", "DeploymentID", "GPUIndexes").
		Where("state NOT IN ? AND sleep_level > 0", []string{proto.InstStopped, proto.InstFailed}).
		Find(&rows).Error
	return rows, err
}
