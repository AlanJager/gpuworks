package planning

import (
	"slices"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A template can use platform facilities that only the node's agent provides:
// it prepares the host side of a mount, or writes a file the container reads.
// Each such facility has one agent capability, and one rule decides what an
// older agent gets: a launch that uses the facility is refused on a node whose
// agent is known not to announce it (ReasonAgentCapabilityMissing), so the
// deployment says why and waits for an upgrade or another node, instead of a
// container that starts without what the template declared.

// launchAgentCapabilities lists the agent capabilities a resolved launch uses,
// each once, in a stable order.
func launchAgentCapabilities(launch deploy.ResolvedLaunch) []string {
	var caps []string
	for _, mount := range launch.Mounts {
		if mount.Purpose == proto.MountPurposeManagedDir {
			caps = append(caps, proto.CapManagedDirs)
			break
		}
	}
	return caps
}

// agentCapabilityRefusal refuses a launch on a node whose agent lacks a
// capability the launch uses. caps is what the agent announced; nil is an
// agent not connected, whose capabilities are unknown, and refuses nothing:
// the controller launches only on connected nodes, and a preview of an
// offline node shows the launch as a current agent would get it.
func agentCapabilityRefusal(caps []string, node *store.Node, launch deploy.ResolvedLaunch) *scheduler.Reason {
	if caps == nil {
		return nil
	}
	for _, capability := range launchAgentCapabilities(launch) {
		if slices.Contains(caps, capability) {
			continue
		}
		params := map[string]any{"capability": capability}
		if node != nil {
			params["node"], params["node_uuid"] = node.DisplayName(), node.UUID
		}
		return &scheduler.Reason{Code: scheduler.ReasonAgentCapabilityMissing, Params: params}
	}
	return nil
}
