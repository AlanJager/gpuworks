package store

import "context"

// OnlineActionRosterNodes returns only the identity needed to route roster
// installation. A stale online row is harmless: the current session check in
// the Hub still has to succeed before a delivery can be acknowledged.
func (s *Store) OnlineActionRosterNodes(ctx context.Context) ([]Node, error) {
	var nodes []Node
	err := s.db.WithContext(ctx).Model(&Node{}).Select("id", "uuid", "token").Where("state = ?", NodeStateOnline).Order("id").Find(&nodes).Error
	return nodes, err
}
