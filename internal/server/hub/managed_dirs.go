package hub

import (
	"slices"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ReleaseDeploymentDirs tells every connected agent that can hold managed
// directories (proto.CapManagedDirs) that the deployment is gone, so each one
// holding a group of it removes the group (ADR 0077). Which nodes hold one is
// only known to the nodes - an instance that ran there may have moved long
// ago - so the message goes to all of them; it is small and sent once per
// deleted deployment that declared directories. An agent that misses it
// (offline, the push refused) learns the same from its next connection's
// replay (proto.DesiredState.ReleasedDirGroups). It returns how many sessions
// the update was handed to.
func (h *Hub) ReleaseDeploymentDirs(deploymentID uint) int {
	if deploymentID == 0 {
		return 0
	}
	h.mu.RLock()
	sessions := make([]*Session, 0, len(h.sessions))
	for _, session := range h.sessions {
		if slices.Contains(session.caps, proto.CapManagedDirs) {
			sessions = append(sessions, session)
		}
	}
	h.mu.RUnlock()
	sent := 0
	for _, session := range sessions {
		if err := session.PushUpdate(proto.StateUpdate{ReleasedDirDeployments: []uint{deploymentID}}); err != nil {
			h.log.Warn("could not tell the node a deployment's managed directories are released",
				"node", session.NodeUUID, "deployment", deploymentID, "err", err)
			continue
		}
		sent++
	}
	return sent
}
