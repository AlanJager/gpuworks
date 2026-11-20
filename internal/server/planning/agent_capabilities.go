package planning

import (
	"slices"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/placement"
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
	if launch.Peers != nil {
		caps = append(caps, proto.CapPeersFile)
	}
	return caps
}

// peersForAgent is the peers a rank launches with. A template that references
// ${GPUWORKS_PEERS_FILE} needs the file, so it keeps them and an agent that
// does not write peers files is refused (agentCapabilityRefusal). Every other
// multi-node rank is given the file where its agent writes one and goes
// without it elsewhere, as a multi-node launch did before the file existed;
// the group's list still names that rank.
func peersForAgent(req LaunchRequest) []proto.PeerRank {
	if len(req.Topology.Peers) == 0 || req.AgentCaps == nil || PeersFileSupported(req.AgentCaps) || referencesPeersFile(req) {
		return req.Topology.Peers
	}
	return nil
}

// referencesPeersFile reports whether a launch names its peers file through
// the launch variable: in the template's launch fields under either spelling
// (placement.Canonical), or in a manual command, which expands only the
// prefixed one. The system-layer env a program might read is not a
// reference: a launch that only reads the env goes without the file on an
// older agent, which it can test for.
func referencesPeersFile(req LaunchRequest) bool {
	selection := req.Selection
	fields := append(append(append([]string{}, selection.ExtraArgs...), selection.Entrypoint...), selection.PreStartArgv...)
	fields = append(fields, selection.PreStartScript)
	for key, value := range selection.Env {
		fields = append(fields, key, value)
	}
	for _, name := range placement.References(fields...) {
		if placement.Canonical(name) == placement.VarPeersFile {
			return true
		}
	}
	if deploy.CommandMode(req.Source.Overrides.CommandMode) == deploy.CommandManual {
		for _, name := range placement.References(req.Source.Overrides.CmdDisplay) {
			if name == placement.VariablePrefix+placement.VarPeersFile {
				return true
			}
		}
	}
	return false
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
