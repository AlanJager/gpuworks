package store

import "time"

// NodesByUUID reads the named nodes as ListNodes reads every node: with their
// GPUs and credential flags, newest first. A UUID with no node is skipped.
func (s *Store) NodesByUUID(uuids []string) ([]Node, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	var nodes []Node
	err := preloadCards(s.reader()).Where("uuid IN ?", uuids).Order("id desc").Find(&nodes).Error
	if err == nil {
		err = s.markNodeCredentialFlags(nodes)
	}
	return nodes, err
}

// CountNodes counts every node, whatever its state.
// CountNodesUnseenSince counts the nodes whose agents have not reported since
// at (or never have): after a server start, the ones still reconnecting.
func (s *Store) CountNodesUnseenSince(at time.Time) (int, error) {
	var n int64
	err := s.reader().Model(&Node{}).Where("last_seen_at IS NULL OR last_seen_at < ?", at).Count(&n).Error
	return int(n), err
}

func (s *Store) CountNodes() (int, error) {
	var n int64
	err := s.reader().Model(&Node{}).Count(&n).Error
	return int(n), err
}

// labelNodeColumns are the node columns its labels are derived from
// (planning.SystemLabels and the written labels) plus those network
// resolution reads, for a requirement naming a network.
var labelNodeColumns = append([]string{"gpu_vendor", "framework", "arch"}, networkNodeColumns...)

// NodesForLabels reads every node, with its GPUs when withGPUs, but only the
// columns its labels are derived from, newest first. Counting how many nodes
// carry a requirement's labels needs the whole fleet; reading every column
// and card of it on each deployment create or read cost more than the rest
// of the request. Only the card labels need the GPUs.
func (s *Store) NodesForLabels(withGPUs bool) ([]Node, error) {
	var nodes []Node
	q := s.reader().Select(labelNodeColumns).Order("id desc")
	if withGPUs {
		q = preloadCards(q)
	}
	err := q.Find(&nodes).Error
	return nodes, err
}

// NodeNamesByID is the display name (Node.DisplayName) of the nodes of ids,
// one query of the columns it is made from; an unknown id is absent.
func (s *Store) NodeNamesByID(ids []uint) (map[uint]string, error) {
	out := map[uint]string{}
	if len(ids) == 0 {
		return out, nil
	}
	var nodes []Node
	if err := s.reader().Select("id", "uuid", "name", "hostname").Where("id IN ?", ids).Find(&nodes).Error; err != nil {
		return nil, err
	}
	for _, n := range nodes {
		out[n.ID] = n.DisplayName()
	}
	return out, nil
}

// NodesForLabelsByID is NodesForLabels for the named nodes only, in id order:
// one query however many there are, for a caller matching a handful of
// candidates against selectors (sync groups, pull source rules). Unknown ids
// are skipped.
func (s *Store) NodesForLabelsByID(ids []uint, withGPUs bool) ([]Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var nodes []Node
	q := s.reader().Select(labelNodeColumns).Where("id IN ?", ids).Order("id")
	if withGPUs {
		q = preloadCards(q)
	}
	err := q.Find(&nodes).Error
	return nodes, err
}
