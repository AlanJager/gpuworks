package network

import (
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Outcome of attaching one node in a batch.
const (
	AttachAttached = "attached"
	AttachAlready  = "already"
	AttachFailed   = "failed"
)

// AttachResult is one node of a batch attach. Err is set when Status is
// AttachFailed.
type AttachResult struct {
	NodeUUID string
	Status   string
	Err      error
}

// AttachNodes attaches a network to each listed node. Nodes are independent:
// one that fails (an unknown node) does not stop the others, and a node
// already attached counts as done so a retry is harmless.
func (s *Service) AttachNodes(networkID uint, uuids []string) ([]AttachResult, error) {
	if !s.Dedicated() {
		return nil, ErrDedicatedUnavailable
	}
	if _, err := s.store.GetNetwork(networkID); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []AttachResult
	for _, uuid := range uuids {
		uuid = strings.TrimSpace(uuid)
		if uuid == "" || seen[uuid] {
			continue
		}
		seen[uuid] = true
		res := AttachResult{NodeUUID: uuid, Status: AttachAttached}
		_, err := s.Attach(networkID, AttachmentSpec{NodeUUID: uuid})
		switch {
		case errors.Is(err, store.ErrNetworkAttachmentExists):
			res.Status = AttachAlready
		case err != nil:
			res.Status, res.Err = AttachFailed, err
		}
		out = append(out, res)
	}
	return out, nil
}

// HCA edits on an RDMA network.
const (
	HCAAdd    = "add"    // add one port to one node
	HCARemove = "remove" // remove one port from every node that uses it, or from one node
	HCAReset  = "reset"  // let one node's rails be inferred again
)

// HCAEdit changes the rails nodes use on an RDMA network. The first edit of a
// node pins its current rails (inferred ones included) and applies the
// change; until then its rails follow its inventory.
type HCAEdit struct {
	Op       string
	HCA      string // "device" or "device:port"; add and remove
	NodeUUID string // add and reset; remove from this node only when set
}

// Why an HCA edit is refused.
var (
	// ErrNotRDMA: only RDMA networks have rails.
	ErrNotRDMA = errors.New("only RDMA networks have HCAs")
	// ErrNodeNotOnNetwork: the node does not use the network.
	ErrNodeNotOnNetwork = errors.New("node is not on this network")
	// ErrHCANotOnNode: the node reported no such RDMA port.
	ErrHCANotOnNode = errors.New("node has no such RDMA port")
	// ErrHCANotUsed: no node on the network uses the port being removed.
	ErrHCANotUsed = errors.New("no node on this network uses this HCA")
)

// LastRailError refuses removing the last rail of a node; detaching the node
// is how it leaves the network.
type LastRailError struct{ Node string }

func (e *LastRailError) Error() string {
	return "removing this HCA would leave node " + e.Node + " without rails; detach the node instead"
}

// EditHCAs applies one HCA edit. Every affected node changes or none does.
func (s *Service) EditHCAs(networkID uint, edit HCAEdit) error {
	if !s.Dedicated() {
		return ErrDedicatedUnavailable
	}
	network, err := s.store.GetNetwork(networkID)
	if err != nil {
		return err
	}
	if !netplan.UsesRails(network.Type) {
		return ErrNotRDMA
	}
	st, err := s.State()
	if err != nil {
		return err
	}
	onNetwork := map[uint]netplan.Resolved{}
	for _, r := range st.Resolved {
		if r.Network.ID == networkID {
			onNetwork[r.NodeID] = r
		}
	}
	byUUID := map[string]store.Node{}
	byID := map[uint]store.Node{}
	for _, n := range st.Nodes {
		byUUID[n.UUID], byID[n.ID] = n, n
	}
	target := func() (store.Node, netplan.Resolved, error) {
		node, ok := byUUID[strings.TrimSpace(edit.NodeUUID)]
		if !ok {
			return store.Node{}, netplan.Resolved{}, ErrNodeNotOnNetwork
		}
		r, ok := onNetwork[node.ID]
		if !ok {
			return store.Node{}, netplan.Resolved{}, ErrNodeNotOnNetwork
		}
		return node, r, nil
	}
	pins := map[uint][]string{}
	switch edit.Op {
	case HCAAdd:
		node, r, err := target()
		if err != nil {
			return err
		}
		port, ok := portOf(node, network.Type, edit.HCA)
		if !ok {
			return ErrHCANotOnNode
		}
		if contains(r.HCAs, port) {
			return nil
		}
		pins[node.ID] = append(append([]string(nil), r.HCAs...), port)
	case HCARemove:
		only := strings.TrimSpace(edit.NodeUUID)
		if only != "" {
			if _, _, err := target(); err != nil {
				return err
			}
		}
		for nodeID, r := range onNetwork {
			node := byID[nodeID]
			if only != "" && node.UUID != only {
				continue
			}
			port, ok := portOf(node, network.Type, edit.HCA)
			if !ok || !contains(r.HCAs, port) {
				// A pinned port the node no longer reports is matched by name.
				port = strings.TrimSpace(edit.HCA)
				if !contains(r.HCAs, port) {
					continue
				}
			}
			var kept []string
			for _, hca := range r.HCAs {
				if hca != port {
					kept = append(kept, hca)
				}
			}
			if len(kept) == 0 {
				return &LastRailError{Node: node.DisplayName()}
			}
			pins[nodeID] = kept
		}
		if len(pins) == 0 {
			return ErrHCANotUsed
		}
	case HCAReset:
		node, _, err := target()
		if err != nil {
			return err
		}
		pins[node.ID] = nil
	default:
		return &InvalidError{Kind: InvalidOverride, Field: "op", Value: edit.Op}
	}
	return s.store.PinNetworkRails(networkID, pins)
}

// portOf names a rail the node reported for a network of the given type: on
// a host RDMA network "device" (its first port) or "device:port", on an
// accelerator one the card's name.
func portOf(node store.Node, networkType, hca string) (string, bool) {
	hca = strings.TrimSpace(hca)
	if !hcaNamePattern.MatchString(hca) {
		return "", false
	}
	device, _, hasPort := strings.Cut(hca, ":")
	for _, rail := range netplan.AllRails(Inventory(node), networkType) {
		if networkType == netplan.TypeAcceleratorRDMA {
			if rail.Name == hca {
				return rail.Name, true
			}
			continue
		}
		if strings.HasPrefix(rail.Name, device+":") && (!hasPort || rail.Name == hca) {
			return rail.Name, true
		}
	}
	return "", false
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
