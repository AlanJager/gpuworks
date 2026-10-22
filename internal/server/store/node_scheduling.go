package store

import "fmt"

// NodeScheduling is whether a node takes new replicas. It is the
// operator's to set, with one exception: a node whose container runtime
// cannot give a container its accelerators is cordoned by the platform,
// because every deployment on it would fail at container start while its
// cards look perfectly fine (Node.RuntimeGPUBlocked). The operator can
// still turn scheduling back on — they are shown why it was turned off and
// may know better — and the platform will cordon it again on the next
// report while the cause remains. Once a report shows the cause gone, the
// platform turns scheduling back on itself.
type NodeScheduling string

const (
	// NodeSchedulable takes new replicas. It is the zero value.
	NodeSchedulable NodeScheduling = ""
	// NodeCordoned takes no new replica; what runs there keeps running.
	NodeCordoned NodeScheduling = "cordoned"
	// NodeDraining is cordoned, and the replicas a pool placed there are
	// moved: each is replaced on another node before it is retired.
	NodeDraining NodeScheduling = "draining"
)

// Valid reports whether s is one of the scheduling states.
func (s NodeScheduling) Valid() bool {
	switch s {
	case NodeSchedulable, NodeCordoned, NodeDraining:
		return true
	}
	return false
}

// TakesReplicas reports whether a new replica may be launched on the node.
func (s NodeScheduling) TakesReplicas() bool { return s == NodeSchedulable }

// DrainingNodeIDs is the set of nodes being drained. It is read on every
// reconcile of a pool-placed deployment, and is almost always empty.
func (s *Store) DrainingNodeIDs() (map[uint]bool, error) {
	var ids []uint
	if err := s.reader().Model(&Node{}).Where("scheduling = ?", NodeDraining).Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// SetNodeScheduling sets whether a node takes new replicas (see
// NodeScheduling). A change is published like any node change other than a
// routine status report, so every deployment is reconciled: one waiting for
// room when a node is uncordoned, one with a replica to move when it drains.
func (s *Store) SetNodeScheduling(uuid string, scheduling NodeScheduling) (*Node, error) {
	if !scheduling.Valid() {
		return nil, fmt.Errorf("node scheduling %q is not one of schedulable, cordoned, draining", scheduling)
	}
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("scheduling", scheduling)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	s.publishNode(uuid, NodeChangeScheduling)
	return s.NodeWithGPUs(uuid)
}

// NodeChangeScheduling is the change reason of a cordon, a drain or their
// end.
const NodeChangeScheduling = "scheduling"
