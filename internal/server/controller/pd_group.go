package controller

import (
	"context"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// pdRankLaunch is one rank of a PD unit's generation as the launch needs it:
// the unit (its PD placement entry) and the rank's place in it, and, once
// reserved, the rank's ports, the host ports it holds and its PD record.
type pdRankLaunch struct {
	unit  planning.PDUnit
	role  string
	rank  planning.GroupRank
	ports planning.PDRankPorts
	held  []int
	pd    planning.PDRecord
	// unitRecord is set on a leader: the unit it was launched for.
	unitRecord *planning.UnitRecord
}

// launchPDGeneration launches one generation of a PD unit: the leader with
// its API port, side channel block and, on a multi-node unit, the port its
// ranks meet on, then each follower with its own side channel block, meeting
// the leader there. Like launchGeneration it plans every rank before writing
// any, and reports whether the generation was written.
func (c *Controller) launchPDGeneration(ctx context.Context, deployment *unitDeployment, model *store.Model, slot planning.UnitSlot, nodes []*store.Node, attempt int) (bool, error) {
	dep, pd := deployment.dep, deployment.pd
	role := slot.Unit.Role
	units := pd.placement.Units(role)
	if surges := deployment.surgeUnits[role]; len(surges) > 0 {
		units = append(append([]planning.PDUnit(nil), units...), surges...)
	}
	if slot.Unit.Index >= len(units) {
		return false, fmt.Errorf("deployment %d: %s unit %d is not in the placement", dep.ID, role, slot.Unit.Index)
	}
	unit := units[slot.Unit.Index]
	rank := planning.GroupRank{Ranks: len(nodes), Attempt: attempt}
	if slot.Unit.MultiNode() {
		rank.Group = fmt.Sprintf("pd-%d-%s", dep.ID, auth.NewUUID()[:8])
	}
	sideSize := pd.mode.ExtraPorts(role, pd.placement.TP(role))

	leader := pdRankLaunch{unit: unit, role: role, rank: rank, pd: planning.PDRecord{Role: role},
		unitRecord: &planning.UnitRecord{Key: slot.Key, Followers: slot.Unit.FollowersKey()}}
	blocks := []int{1}
	if sideSize > 0 {
		blocks = append(blocks, sideSize)
	}
	if slot.Unit.MultiNode() {
		blocks = append(blocks, 1)
	}
	ports, rejected := c.pickPorts(nodes[0], blocks...)
	if rejected != nil {
		c.recordRankRefusal(dep, rejected, rank, nodes[0])
		return false, nil
	}
	leader.ports.API = ports[0]
	leader.held = []int{ports[0]}
	if sideSize > 0 {
		leader.ports.SideChannel = ports[1]
		leader.held = append(leader.held, portBlock(ports[1], sideSize)...)
	}
	if slot.Unit.MultiNode() {
		leader.ports.Master = ports[len(ports)-1]
		leader.held = append(leader.held, leader.ports.Master)
	}
	// The whole generation is planned before any rank is written; see
	// launchGeneration.
	launch, record, refusal, err := c.planPDRank(ctx, deployment, nodes[0], model, leader)
	if err != nil {
		return false, err
	}
	if launch == nil {
		c.recordRankRefusal(dep, refusal, leader.rank, nodes[0])
		return false, nil
	}
	launches := []rankLaunch{*launch}
	for index, node := range nodes[1:] {
		follower := pdRankLaunch{unit: unit, role: role, rank: rank, pd: planning.PDRecord{Role: role}}
		follower.rank.Rank = index + 1
		follower.ports = planning.PDRankPorts{Master: record.MasterPort, LeaderAddress: record.Address}
		if sideSize > 0 {
			ports, rejected := c.pickPorts(node, sideSize)
			if rejected != nil {
				c.recordRankRefusal(dep, rejected, follower.rank, node)
				return false, nil
			}
			follower.ports.SideChannel = ports[0]
			follower.held = portBlock(follower.ports.SideChannel, sideSize)
		}
		launch, _, refusal, err := c.planPDRank(ctx, deployment, node, model, follower)
		if err != nil {
			return false, err
		}
		if launch == nil {
			c.recordRankRefusal(dep, refusal, follower.rank, node)
			return false, nil
		}
		launches = append(launches, *launch)
	}
	return true, c.persistGeneration(ctx, dep, launches)
}

// portBlock lists size consecutive ports from first.
func portBlock(first, size int) []int {
	out := make([]int, 0, size)
	for p := first; p < first+size; p++ {
		out = append(out, p)
	}
	return out
}

// planPDRank assembles and checks one rank of a PD unit with the ports its
// caller reserved, without writing it, and returns the record it launches
// with. A nil launch means the rank cannot start now (see planRank).
func (c *Controller) planPDRank(ctx context.Context, deployment *unitDeployment, node *store.Node, model *store.Model, launch pdRankLaunch) (*rankLaunch, planning.PDRecord, *scheduler.Reason, error) {
	dep, pd := deployment.dep, deployment.pd
	files, ready, err := c.ensureDeploymentModelsPresent(ctx, node, dep, model)
	if err != nil || !ready {
		return nil, planning.PDRecord{}, nil, err
	}
	req, templateLayer, modelHostPath, err := c.launchRequest(dep, node, model, &files)
	if err != nil {
		return nil, planning.PDRecord{}, nil, err
	}
	var compute *netplan.Resolved
	if resolved, ok := pd.group.Ranks[node.ID]; ok {
		compute = &resolved
	}
	topology, err := planning.RankTopology(node, compute, launch.rank)
	if err != nil {
		return nil, planning.PDRecord{}, nil, err
	}
	pd.placement.RankLaunch(pd.mode, launch.role, launch.unit, launch.rank.Rank, topology, launch.ports).Apply(&req)
	plan, refusal, err := c.planInstanceLaunch(ctx, dep, node, req, templateLayer)
	if err != nil || refusal != nil {
		if refusal != nil {
			refusal = c.pinnedGPUsRefusal(dep, node, req.Placement.GPUIndexes, refusal)
		}
		return nil, planning.PDRecord{}, refusal, err
	}
	record := launch.pd
	record.Address, record.Port, record.MasterPort = topology.NodeAddress, launch.ports.SideChannel, launch.ports.Master
	return &rankLaunch{node: node, plan: plan, port: launch.ports.API, modelHostPath: modelHostPath, reads: files.reads(),
		extras: instanceExtras{Ports: launch.held, Role: launch.role, ExplainNetwork: true, PD: &record, Unit: launch.unitRecord}}, record, nil, nil
}
