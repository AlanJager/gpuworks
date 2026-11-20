package deploy

import (
	"gpuworks.ai/gpuworks/internal/proto"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// PeersFileEnv is the container environment variable that names a rank's
// peers file (proto.PeersFileMountPath). It is the launch variable
// ${GPUWORKS_PEERS_FILE} (placement.VarPeersFile) under the same name and
// value, stated in the environment as well so a program the recipe starts
// can find the file without being passed it.
const PeersFileEnv = placementeval.VariablePrefix + placementeval.VarPeersFile

// peersFile is the peers file of one rank of a multi-node group: the group's
// ranks the placement stated (Params.Peers) with this rank, the rendezvous and
// the shared compute network as the launch resolved them. The fields that are
// also launch variables (launchVariables) come from the same Params, and the
// reader's own row states its NodeAddress, so on every rank node_rank,
// nnodes, master_addr, master_port and its own node_addr equal NODE_RANK,
// NNODES, MASTER_ADDR, MASTER_PORT and NODE_ADDR.
//
// Nil when the launch is not a rank of a multi-node group, when the placement
// stated no peers (an agent that would not write the file), and for the
// symbolic command a manual script starts from, which belongs to no rank.
func peersFile(p Params) *proto.PeersFile {
	if p.NNodes <= 1 || len(p.Peers) != p.NNodes || p.NodeRank < 0 || p.NodeRank >= p.NNodes || p.Symbolic {
		return nil
	}
	file := &proto.PeersFile{
		Version: proto.PeersFileVersion, NodeRank: p.NodeRank, NNodes: p.NNodes,
		MasterAddr: p.MasterAddr, MasterPort: p.MasterPort,
		Ranks: make([]proto.PeerRank, len(p.Peers)),
	}
	for i, peer := range p.Peers {
		peer.HCAs = append([]proto.PeerHCA(nil), peer.HCAs...)
		if peer.Management != nil {
			management := *peer.Management
			peer.Management = &management
		}
		file.Ranks[i] = peer
	}
	if p.NodeAddress != "" {
		file.Ranks[p.NodeRank].NodeAddr = p.NodeAddress
	}
	if p.ComputeNetwork.Type != "" {
		file.Network = &proto.PeersNetwork{Type: p.ComputeNetwork.Type, LinkLayer: p.ComputeNetwork.LinkLayer}
	}
	return file
}

// peersEnv is the system-layer variable that tells a rank where its peers
// file is; empty when the launch has none. Template and user layers can
// still override it like any other system key.
func peersEnv(p Params) map[string]string {
	if peersFile(p) == nil {
		return nil
	}
	return map[string]string{PeersFileEnv: proto.PeersFileMountPath}
}

// platformFilePaths are the container paths a launch's mounts must stay clear
// of (validateResolvedMounts) because the agent mounts the platform's own
// files there: all of proto.PlatformFileDir, whatever the mount comes from (a
// deployment row, a template component, asset or dir). A launch of a
// deployment as stored keeps a mount it was created with, before the
// directory was the platform's, unless it now covers the peers file it gets.
func platformFilePaths(peers, stored bool) []string {
	switch {
	case !stored:
		return []string{proto.PlatformFileDir}
	case peers:
		return []string{proto.PeersFileMountPath}
	default:
		return nil
	}
}
