package network

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// This file is the only place stored rows become netplan's model.

// Node converts a stored node. Labels or an inventory that do not decode
// count as none.
func Node(n store.Node) netplan.Node {
	labels, err := store.NodeLabels(n.Labels)
	if err != nil {
		labels = map[string]string{}
	}
	off := map[string]bool{}
	for _, role := range RolesOff(n) {
		off[role] = true
	}
	return netplan.Node{ID: n.ID, UUID: n.UUID, Name: n.DisplayName(), Labels: labels, Inventory: Inventory(n), RolesOff: off}
}

// Nodes converts stored nodes, keeping their order.
func Nodes(nodes []store.Node) []netplan.Node {
	out := make([]netplan.Node, len(nodes))
	for i, n := range nodes {
		out[i] = Node(n)
	}
	return out
}

// Inventory decodes a node's last reported network inventory; nil when its
// agent never reported one or it does not decode.
func Inventory(n store.Node) *proto.NetInventory {
	if strings.TrimSpace(n.NetworkInventory) == "" {
		return nil
	}
	var inv proto.NetInventory
	if err := json.Unmarshal([]byte(n.NetworkInventory), &inv); err != nil {
		return nil
	}
	return &inv
}

// ManagementAddress is a node's reported address on the management network;
// false until its agent reports one.
func ManagementAddress(n store.Node) (proto.ManagementAddress, bool) {
	return Node(n).Management()
}

// RolesOff lists the roles a node's management network may not carry.
func RolesOff(n store.Node) []string {
	return store.DecodeStringList(n.ManagementRolesOff)
}

// Network converts a stored network.
func Network(n store.Network) netplan.Network {
	return netplan.Network{
		ID: n.ID, Name: n.Name, Roles: store.DecodeStringList(n.Roles), Type: n.Type,
		CIDR: n.CIDR, Interface: n.Interface,
	}
}

// Attachment converts a stored attachment.
func Attachment(a store.NetworkAttachment) netplan.Attachment {
	return netplan.Attachment{
		ID: a.ID, NetworkID: a.NetworkID, NodeID: a.NodeID, LabelKey: a.LabelKey, LabelValue: a.LabelValue,
		Interface: a.Interface, IP: a.IP, HCAs: store.DecodeStringList(a.HCAs),
	}
}

func inputs(nodes []store.Node, networks []store.Network, attachments []store.NetworkAttachment) netplan.Inputs {
	in := netplan.Inputs{Nodes: Nodes(nodes)}
	for _, n := range networks {
		in.Networks = append(in.Networks, Network(n))
	}
	for _, a := range attachments {
		in.Attachments = append(in.Attachments, Attachment(a))
	}
	return in
}
