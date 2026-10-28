// Package netplan decides which network each node uses for each role: the
// vocabulary of network roles and types, the resolution of attachments against
// the interfaces a node reported, the management network that carries any role
// nothing else is attached for, and the choices built on it (the network a
// multi-node group shares, the path a copy between two nodes takes, whether a
// node may serve). It is pure: callers hand it decoded rows and read back
// decisions; loading and persisting belong to internal/server/network and
// internal/server/store.
package netplan

import (
	"errors"
	"fmt"
	"strings"
)

// Network roles and link types. The API validates against these and the UI
// receives them from the server dictionary.
const (
	RoleStorageSync = "storage_sync"
	RoleCompute     = "compute"
	RoleServing     = "serving"

	// TypeHostEthernet is a host network interface carrying TCP.
	TypeHostEthernet = "host_ethernet"
	// TypeHostRDMA is the host's RDMA ports, InfiniBand and RoCE alike:
	// which one a node has is read from its ports' link layer, never asked
	// of the operator.
	TypeHostRDMA = "host_rdma"
	// TypeAcceleratorRDMA is the RoCE ports the accelerator cards themselves
	// own (Ascend 910): each rail is a card. The host kernel does not see
	// those ports, so no host process can bind their addresses; the network
	// still resolves a host address, for the handshake alone.
	TypeAcceleratorRDMA = "accelerator_rdma"
	// TypeAcceleratorHCCS is the HCCS/HIXL fabric exposed by Ascend A3/910C
	// accelerators. It uses the same card rails as accelerator_rdma, but a
	// different transfer protocol and launch configuration.
	TypeAcceleratorHCCS = "accelerator_hccs"

	// Link layers a resolved RDMA network reports per node.
	LinkLayerInfiniBand = "infiniband"
	LinkLayerRoCE       = "roce"
	LinkLayerHCCS       = "hccs"
)

// Roles lists every role in display order.
var Roles = []string{RoleStorageSync, RoleCompute, RoleServing}

// Types lists every link type in display order.
var Types = []string{TypeHostEthernet, TypeHostRDMA, TypeAcceleratorRDMA, TypeAcceleratorHCCS}

// Attachment sources, from strongest to weakest. The management network is
// weakest of all: it carries a role only where no network is attached for it.
const (
	SourceNode       = "node"
	SourceLabel      = "label"
	SourceManagement = "management"
)

// Warning codes on a resolved network. They are rendered by the client locale;
// Warning.Params carry the values to interpolate.
const (
	// WarningConflict: another network of the same role matched this node at
	// the same rank and lost because it was attached later.
	WarningConflict = "conflict"
	// WarningOverrideMissing: an override names an interface, IP or HCA the
	// node's last inventory does not contain.
	WarningOverrideMissing = "override_missing"
	// WarningLinkLayerMismatch: nodes of one RDMA network resolved to different
	// link layers (InfiniBand and RoCE), which cannot be one fabric.
	WarningLinkLayerMismatch = "link_layer_mismatch"
	// WarningNoHCA: an RDMA network found no usable RDMA port on the node.
	WarningNoHCA = "rdma_no_hca"
	// WarningBootstrapMissing: an RDMA network resolved no bootstrap address
	// on the node, so nothing can reach it for the handshake.
	WarningBootstrapMissing = "bootstrap_missing"
	// WarningHCAUnusable: a pinned rail is not ACTIVE, or is a RoCE port
	// whose interface has no IPv4 address.
	WarningHCAUnusable = "hca_unusable"
	// WarningHCAMismatch: nodes of one RDMA network use different numbers of
	// rails, so some rails have no peer.
	WarningHCAMismatch = "hca_mismatch"
)

// ErrUnknownRole rejects a role name that is not one of Roles.
var ErrUnknownRole = errors.New("unknown network role")

// UnknownRoleError names the role CanonicalRoles rejected; it matches
// ErrUnknownRole.
type UnknownRoleError struct{ Role string }

func (e *UnknownRoleError) Error() string { return fmt.Sprintf("%v: %q", ErrUnknownRole, e.Role) }

func (e *UnknownRoleError) Unwrap() error { return ErrUnknownRole }

// UsesRails reports whether a network of type t carries its traffic on rails
// (RDMA ports or accelerator cards) apart from the host address it resolves
// for the handshake.
func UsesRails(t string) bool {
	return t == TypeHostRDMA || IsAccelerator(t)
}

// IsAccelerator reports whether t uses accelerator-owned network rails.
func IsAccelerator(t string) bool {
	return t == TypeAcceleratorRDMA || t == TypeAcceleratorHCCS
}

// TypeRoles lists the roles a network of type t may carry, in Roles order.
// An accelerator RDMA network carries compute traffic only: storage sync
// and serving are host processes, which cannot bind a card's address.
func TypeRoles(t string) []string {
	if IsAccelerator(t) {
		return []string{RoleCompute}
	}
	return append([]string(nil), Roles...)
}

// RoleAllowed reports whether a network of type t may carry role.
func RoleAllowed(t, role string) bool {
	for _, r := range TypeRoles(t) {
		if r == role {
			return true
		}
	}
	return false
}

// ValidType reports whether t is one of Types.
func ValidType(t string) bool {
	for _, known := range Types {
		if t == known {
			return true
		}
	}
	return false
}

// CanonicalRoles validates role names and returns them deduplicated in Roles
// order, never nil. Surrounding spaces are ignored. An unknown role is an
// *UnknownRoleError.
func CanonicalRoles(roles []string) ([]string, error) {
	seen := map[string]bool{}
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if !known(role) {
			return nil, &UnknownRoleError{Role: role}
		}
		seen[role] = true
	}
	clean := []string{}
	for _, role := range Roles {
		if seen[role] {
			clean = append(clean, role)
		}
	}
	return clean, nil
}

func known(role string) bool {
	for _, r := range Roles {
		if r == role {
			return true
		}
	}
	return false
}
