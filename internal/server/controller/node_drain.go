package controller

import (
	"errors"
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// drainMessage is what an instance moved off a draining node says.
const drainMessage = "its node is draining; replaced on another node"

// nodeDrain moves a pool-placed deployment's replicas off draining nodes
// without lowering how many serve: a replica there keeps serving while a
// replacement starts on another node (one surge unit), and is released into
// the ordinary draining path only once enough replacements run. One replica
// is moved at a time per deployment.
//
// It returns the units to reconcile - the deployment's own, plus a surge unit
// while a replacement is needed - and insts with any released instance marked
// draining. Multi-node and PD replicas name their nodes: drainNamedUnits
// moves them (node_failover_drain.go).
func (c *Controller) nodeDrain(dep *store.Deployment, units []planning.Unit, insts []store.Instance) ([]planning.Unit, []store.Instance, error) {
	if len(units) == 0 || !units[0].Pool() {
		return units, insts, nil
	}
	draining, err := c.store.DrainingNodeIDs()
	if err != nil || len(draining) == 0 {
		return units, insts, err
	}
	var movable []int
	serving := 0
	for i, inst := range insts {
		if inst.Headless || !planning.InstanceActive(inst.State) {
			continue
		}
		if draining[inst.NodeID] {
			movable = append(movable, i)
			continue
		}
		if inst.State == proto.InstRunning {
			serving++
		}
	}
	if len(movable) == 0 {
		return units, insts, nil
	}
	replicas := max(dep.Replicas, 1)
	// Releasing one leaves serving + len(movable) - 1 replicas taking
	// requests: never fewer than the deployment asks for.
	if serving+len(movable)-1 < replicas {
		surged := append(append([]planning.Unit(nil), units...), planning.Unit{Index: len(units), Ranks: []planning.UnitRank{{}}})
		return surged, insts, nil
	}
	sort.SliceStable(movable, func(a, b int) bool { return insts[movable[a]].CreatedAt.Before(insts[movable[b]].CreatedAt) })
	release := &insts[movable[0]]
	err = c.store.TransitionInstances(dep.ID, []string{release.UUID}, store.ReplicaFillingStates(), proto.InstDraining, drainMessage)
	if errors.Is(err, store.ErrStateChanged) {
		return units, insts, nil
	}
	if err != nil {
		return units, insts, err
	}
	out := append([]store.Instance(nil), insts...)
	out[movable[0]].State = proto.InstDraining
	c.log.Info("replica moved off a draining node", "dep", dep.ID, "instance", release.UUID, "node", release.NodeID)
	return units, out, nil
}
