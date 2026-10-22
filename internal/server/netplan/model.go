package netplan

import "gpuworks.ai/gpuworks/internal/proto"

// Network is a declared node-to-node network. Its selector (CIDR, Interface,
// HCA) is only the default: a node attachment may override it. ID 0 is the
// management network, which has no row.
type Network struct {
	ID    uint
	Name  string
	Roles []string // of Roles
	Type  string   // of Types
	// Selector: the subnet the interface address must be in and an
	// interface name glob. A network that uses rails has no interface glob;
	// its CIDR only picks the bootstrap address, and its rails are inferred
	// (rdma.go).
	CIDR      string
	Interface string
}

// HasRole reports whether the network may carry role.
func (n Network) HasRole(role string) bool {
	for _, r := range n.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Attachment binds a network to one node (NodeID set) or to every node
// carrying a label (LabelKey set). Node attachments outrank label ones; within
// one rank the earliest attachment wins, so attachments are passed in creation
// order.
type Attachment struct {
	ID         uint
	NetworkID  uint
	NodeID     uint
	LabelKey   string
	LabelValue string
	// Overrides, node attachments only. Interface and IP are the bootstrap
	// address on networks that use rails and the data address otherwise.
	// HCAs pin such a network's rails; empty means inferred.
	Interface string
	IP        string
	HCAs      []string // exact rail names: "mlx5_0" or "mlx5_0:1", or a card ("npu3")
}

// Node is what resolution reads about a node.
type Node struct {
	ID     uint
	UUID   string
	Name   string // what a person calls it, for messages
	Labels map[string]string
	// Inventory is the node's last reported interfaces, RDMA ports and
	// management address; nil until its agent reports one.
	Inventory *proto.NetInventory
	// RolesOff are the roles its management network may not carry.
	RolesOff map[string]bool
}

// Management is the node's address on the management network: the address
// its agent reaches the server from. False until the agent reports one.
func (n Node) Management() (proto.ManagementAddress, bool) {
	if n.Inventory != nil && n.Inventory.Management != nil && n.Inventory.Management.IP != "" {
		return *n.Inventory.Management, true
	}
	return proto.ManagementAddress{}, false
}

// Inputs is everything Resolve reads.
type Inputs struct {
	Nodes       []Node
	Networks    []Network
	Attachments []Attachment // in creation order
}

// Warning is one reason to flag a resolved network on a node.
type Warning struct {
	Code   string            `json:"code"`
	Params map[string]string `json:"params,omitempty"`
}

// Resolved is the network a node uses for one role, with the host endpoints
// picked from the node's inventory. Interface and IP are empty when nothing on
// the node matches.
type Resolved struct {
	NodeID       uint
	Role         string
	Network      Network
	AttachmentID uint
	Source       string // SourceNode | SourceLabel | SourceManagement
	LabelKey     string
	LabelValue   string
	Interface    string
	IP           string
	HCAs         []string // rails: "device:port", or cards ("npu3") on an accelerator RDMA network
	// LinkLayer is what the rails are: LinkLayerInfiniBand or
	// LinkLayerRoCE; empty for Ethernet networks or when no port matched.
	LinkLayer string
	// HCAsPinned and BootstrapPinned are set when the node attachment pins
	// the RDMA network's rails or bootstrap instead of leaving them inferred.
	HCAsPinned      bool
	BootstrapPinned bool
	Warnings        []Warning
}

// IsManagement reports whether the management network carries this role.
func (r Resolved) IsManagement() bool { return r.Source == SourceManagement }

// Resolution is the outcome of Resolve for a set of nodes.
type Resolution []Resolved

// For is the network a node resolved for one role, if any.
func (rs Resolution) For(nodeID uint, role string) (Resolved, bool) {
	for _, r := range rs {
		if r.NodeID == nodeID && r.Role == role {
			return r, true
		}
	}
	return Resolved{}, false
}

// Of lists what one node resolved, in role order.
func (rs Resolution) Of(nodeID uint) []Resolved {
	var out []Resolved
	for _, r := range rs {
		if r.NodeID == nodeID {
			out = append(out, r)
		}
	}
	return out
}

// TestedRole reports whether internal network tests cover a role: storage sync
// and compute traffic flow node to node. Serving flows from the server to a
// node, which the pre-start serving probe checks instead.
func TestedRole(role string) bool {
	return role == RoleStorageSync || role == RoleCompute
}

// Testable reports whether internal tests cover any role of the network.
func (n Network) Testable() bool {
	for _, role := range n.Roles {
		if TestedRole(role) {
			return true
		}
	}
	return false
}
