package planning

import "sync"

// An operator scaling a single-node deployment up may name the nodes its new
// replicas start on. The hint covers only their first placement: the
// controller tries the named node first when it next starts a replica, and
// afterwards the replica is an ordinary one, rebuilt and moved like any other.
// Hints live in memory; a server restart before the replicas start leaves
// them to the candidate pool.
var placementHints = struct {
	sync.Mutex
	byDeployment map[uint][]string
}{byDeployment: map[uint][]string{}}

// HintPlacement queues node UUIDs for the deployment's next replica starts,
// replacing what was queued before.
func HintPlacement(deploymentID uint, nodes []string) {
	placementHints.Lock()
	defer placementHints.Unlock()
	if len(nodes) == 0 {
		delete(placementHints.byDeployment, deploymentID)
		return
	}
	placementHints.byDeployment[deploymentID] = append([]string(nil), nodes...)
}

// NextPlacementHint is the node the deployment's next replica should try
// first, if one is queued.
func NextPlacementHint(deploymentID uint) (string, bool) {
	placementHints.Lock()
	defer placementHints.Unlock()
	queued := placementHints.byDeployment[deploymentID]
	if len(queued) == 0 {
		return "", false
	}
	return queued[0], true
}

// ConsumePlacementHint drops the first queued node once a replica start has
// tried it, whether it took the replica or not.
func ConsumePlacementHint(deploymentID uint, node string) {
	placementHints.Lock()
	defer placementHints.Unlock()
	queued := placementHints.byDeployment[deploymentID]
	if len(queued) == 0 || queued[0] != node {
		return
	}
	if len(queued) == 1 {
		delete(placementHints.byDeployment, deploymentID)
		return
	}
	placementHints.byDeployment[deploymentID] = queued[1:]
}
