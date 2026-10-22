package store

import "strings"

// SetNodeManagementRolesOff records the roles a node's management network may
// not carry; the caller passes them validated. Only newly started instances
// and later syncs see the change.
func (s *Store) SetNodeManagementRolesOff(uuid string, off []string) error {
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("management_roles_off", EncodeStringList(off))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "management_roles")
	return nil
}

// DisplayName is what a person calls a node: the name an operator gave it,
// then its reported hostname, and only as a last resort a short uuid prefix.
// Node identity on the wire stays the uuid; this is presentation only.
func (n Node) DisplayName() string {
	if name := strings.TrimSpace(n.Name); name != "" {
		return name
	}
	if host := strings.TrimSpace(n.Hostname); host != "" {
		return host
	}
	if uuid := strings.TrimSpace(n.UUID); len(uuid) > 8 {
		return uuid[:8]
	}
	return strings.TrimSpace(n.UUID)
}
