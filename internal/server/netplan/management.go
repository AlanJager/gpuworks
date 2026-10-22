package netplan

// ManagementName names the built-in management network.
const ManagementName = "management"

// Management is the built-in network every node is on: the address it reaches
// the server from. It has no row; its ID is 0 and it cannot be edited or
// deleted. Each node may turn off the roles it carries.
func Management() Network {
	return Network{Name: ManagementName, Type: TypeHostEthernet, Roles: append([]string(nil), Roles...)}
}

// managementFor is the management network carrying a role on a node, unless
// the node turned the role off or has not reported its management address.
func managementFor(node Node, role string) (Resolved, bool) {
	if node.RolesOff[role] {
		return Resolved{}, false
	}
	return managementEndpoint(node, role)
}

// ManagementEndpoint is a node's management network as a resolved network,
// whatever roles it carries there: internal tests probe the management network
// itself. False until the node's agent reports its address.
func ManagementEndpoint(node Node) (Resolved, bool) {
	return managementEndpoint(node, RoleCompute)
}

func managementEndpoint(node Node, role string) (Resolved, bool) {
	addr, ok := node.Management()
	if !ok {
		return Resolved{}, false
	}
	return Resolved{
		NodeID: node.ID, Role: role, Network: Management(), Source: SourceManagement,
		Interface: addr.Interface, IP: addr.IP,
	}, true
}
