package store

import "context"

// nodeNamesSQL reads the named nodes by primary key.
const nodeNamesSQL = `SELECT id, uuid, name, hostname FROM nodes WHERE id IN ?`

// nodeNameRow is one node row NodeNamesByID reads.
type nodeNameRow struct {
	ID       uint
	UUID     string
	Name     string
	Hostname string
}

// NodeNamesByID is the display name (Node.DisplayName: the name an operator
// gave the node, otherwise its hostname) of each named node that exists,
// keyed by id; a node that does not exist (never did, or was deleted) is
// absent. Duplicate and zero ids count once or not at all. No ids is an
// empty map without a read; more than MaxNodeReadBatch distinct ids is
// ErrNodeReadBatch, refused rather than cut short. It is one primary-key
// query on the read pool.
func (s *Store) NodeNamesByID(ctx context.Context, ids []uint) (map[uint]string, error) {
	ids = uniqueIDs(ids)
	if len(ids) > MaxNodeReadBatch {
		return nil, ErrNodeReadBatch
	}
	if len(ids) == 0 {
		return map[uint]string{}, nil
	}
	var rows []nodeNameRow
	if err := s.reader().WithContext(ctx).Raw(nodeNamesSQL, ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]string, len(rows))
	for _, r := range rows {
		out[r.ID] = Node{UUID: r.UUID, Name: r.Name, Hostname: r.Hostname}.DisplayName()
	}
	return out, nil
}
