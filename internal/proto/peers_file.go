package proto

import (
	"path"
	"strings"
)

// PlatformFileDir is the container directory the platform keeps for the files
// it provides to a launch: the pre-start wrapper (pre-start.sh) and the peers
// file (peers.json). No mount a deployment or a template states may be it,
// lie inside it or contain it (PlatformFilePath), so a platform file can never
// be shadowed and a new one never collides with a recipe's own mount.
const PlatformFileDir = "/gpuworks"

// PeersFileMountPath is where the agent mounts a launch's peers file,
// read-only. The server names it to the container in GPUWORKS_PEERS_FILE
// (deploy.PeersFileEnv), only on a launch that has a peers file, so a recipe
// can test for the variable.
const PeersFileMountPath = PlatformFileDir + "/peers.json"

// PlatformFilePath reports whether a container path overlaps PlatformFileDir:
// is it, lies inside it, or contains it (only "/", which no mount may be).
func PlatformFilePath(target string) bool {
	target = path.Clean(strings.TrimSpace(target))
	return target == PlatformFileDir || strings.HasPrefix(target, PlatformFileDir+"/") || target == "/"
}

// PeersFileVersion is the shape of PeersFile the server writes. A reader
// checks it: a field whose meaning changes gets a new version, while a field
// that is only added keeps the version (readers ignore keys they do not know).
const PeersFileVersion = 1

// CapPeersFile: the agent writes ResolvedLaunch.Peers to a host file and
// mounts it read-only at PeersFileMountPath. The server states a peers file
// only to an agent that advertises it; an older agent would leave
// GPUWORKS_PEERS_FILE naming a file that is not there.
const CapPeersFile = "launch.peers_file"

// PeersFile is the document every rank of a multi-node instance group reads
// from GPUWORKS_PEERS_FILE: every rank of the group, where it is, and the
// rails of the compute network it is reached on. It is what a recipe needs to
// build its own rendezvous or rank table (a fleet.conf, an HCCL rank table, a
// Mooncake configuration) without a discovery step of its own. The document
// is identical on every rank except NodeRank, which names the reader.
//
// Its names are the launch variables' (NODE_RANK, NNODES, NODE_ADDR,
// MASTER_ADDR, MASTER_PORT, in lower case) and the compute network's (a
// node's interface and hcas, each rail's name, netdev and ip, as the network
// API states them), and every value that is also a launch variable equals
// that variable on the rank it describes. It states facts the server decided
// or the agents reported when the group was placed; it does not state what a
// container probes for itself, such as a RoCE GID index.
// docs/reference/deployment-launch-sources.md documents the shape.
type PeersFile struct {
	Version int `json:"version"`
	// NodeRank is the reader's own rank (NODE_RANK); rank 0 leads.
	NodeRank int `json:"node_rank"`
	// NNodes is the number of ranks (NNODES); it equals len(Ranks).
	NNodes int `json:"nnodes"`
	// MasterAddr and MasterPort are the rendezvous every rank meets at
	// (MASTER_ADDR, MASTER_PORT); omitted when the launch has none.
	MasterAddr string `json:"master_addr,omitempty"`
	MasterPort int    `json:"master_port,omitempty"`
	// Network is the compute network the group shares; nil when the group
	// runs over the management network.
	Network *PeersNetwork `json:"network,omitempty"`
	Ranks   []PeerRank    `json:"ranks"`
}

// PeersNetwork is the compute network a group shares.
type PeersNetwork struct {
	// Type is the network's type: "host_rdma", "host_ethernet",
	// "accelerator_rdma" or "accelerator_hccs".
	Type string `json:"type"`
	// LinkLayer is "infiniband", "roce" or "hccs" when the rails are RDMA or
	// accelerator ports; empty for Ethernet.
	LinkLayer string `json:"link_layer,omitempty"`
}

// PeerRank is one rank of the group.
type PeerRank struct {
	NodeRank int `json:"node_rank"`
	// NodeUUID and NodeName are the node's identity and name in the platform;
	// Hostname is what the node reports as its host name.
	NodeUUID string `json:"node_uuid"`
	NodeName string `json:"node_name"`
	Hostname string `json:"hostname,omitempty"`
	// NodeAddr is the address the rank advertises to the group: its NODE_ADDR.
	NodeAddr string `json:"node_addr"`
	// Management is the node's address on the management network; nil when
	// its agent has not reported one.
	Management *ManagementAddress `json:"management,omitempty"`
	// Interface is the host interface that carries the group's bootstrap
	// traffic on this node; empty when none was resolved.
	Interface string `json:"interface,omitempty"`
	// HCAs are the compute network's rails on this node, in rail order (the
	// order NCCL_IB_HCA names them): RDMA ports on a host RDMA network, the
	// cards' ports on an accelerator network; empty on an Ethernet network.
	HCAs []PeerHCA `json:"hcas,omitempty"`
}

// PeerHCA is one rail of a rank's compute network.
type PeerHCA struct {
	// Name is the rail as the platform names it: "device:port" for an RDMA
	// port (rocep1s0f1:1), the card for an accelerator port (npu3).
	Name string `json:"name"`
	// Device and Port are an RDMA rail's device and port, Name split; empty
	// for a card.
	Device string `json:"device,omitempty"`
	Port   int    `json:"port,omitempty"`
	// NetDev is the host interface that holds the rail's address (for RoCE
	// the one whose address the port's GID table carries, which can be a
	// bridge or bond above the port); empty for InfiniBand without IPoIB and
	// for accelerator cards.
	NetDev string `json:"netdev,omitempty"`
	// IP is the rail's own IPv4 address: the RoCE address verified against
	// the port's GID table (else its interface's), the IPoIB address, or the
	// card's port address. Empty when the node reported none.
	IP string `json:"ip,omitempty"`
}
