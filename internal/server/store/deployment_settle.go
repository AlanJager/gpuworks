package store

import "gpuworks.ai/gpuworks/internal/proto"

// DeploymentSettleFact is what says whether a deployment could gain anything
// from a node's routine status report: its desired state and shape, and how
// many of its instances run or are on their way somewhere.
type DeploymentSettleFact struct {
	ID        uint
	State     DeploymentState
	Replicas  int
	NNodes    int
	Placement string
	// Running counts instances in the running state; Moving those pending,
	// pulling, starting, optimizing, draining or being removed. Stopped and
	// failed rows count in neither.
	Running int
	Moving  int
	// OnDrainingNode counts instances not stopped or failed on a node being
	// drained: a replica still to be moved.
	OnDrainingNode int
}

// DeploymentSettleFacts reads every deployment's settle facts in three
// queries whatever the number of deployments and instances.
func (s *Store) DeploymentSettleFacts() ([]DeploymentSettleFact, error) {
	var deps []Deployment
	if err := s.reader().Select("ID", "State", "Replicas", "NNodes", "Placement").Order("id").Find(&deps).Error; err != nil {
		return nil, err
	}
	var counts []struct {
		DeploymentID uint
		State        string
		N            int
	}
	if err := s.reader().Model(&Instance{}).Select("deployment_id, state, count(*) AS n").Group("deployment_id, state").Scan(&counts).Error; err != nil {
		return nil, err
	}
	var draining []struct {
		DeploymentID uint
		N            int
	}
	if err := s.reader().Table("instances").Select("instances.deployment_id, count(*) AS n").
		Joins("JOIN nodes ON nodes.id = instances.node_id").
		Where("nodes.scheduling = ? AND instances.state NOT IN ?", NodeDraining, []string{proto.InstStopped, proto.InstFailed}).
		Group("instances.deployment_id").Scan(&draining).Error; err != nil {
		return nil, err
	}
	onDraining := make(map[uint]int, len(draining))
	for _, d := range draining {
		onDraining[d.DeploymentID] = d.N
	}
	running, moving := map[uint]int{}, map[uint]int{}
	for _, c := range counts {
		switch c.State {
		case proto.InstRunning:
			running[c.DeploymentID] += c.N
		case proto.InstStopped, proto.InstFailed:
		default:
			moving[c.DeploymentID] += c.N
		}
	}
	out := make([]DeploymentSettleFact, 0, len(deps))
	for _, d := range deps {
		out = append(out, DeploymentSettleFact{
			ID: d.ID, State: d.State, Replicas: d.Replicas, NNodes: d.NNodes, Placement: d.Placement,
			Running: running[d.ID], Moving: moving[d.ID], OnDrainingNode: onDraining[d.ID],
		})
	}
	return out, nil
}
