package store

import (
	"fmt"
	"sort"

	"gorm.io/gorm"
)

// ManagementRolesNotFoundError names a node of a batch that does not exist.
type ManagementRolesNotFoundError struct{ UUID string }

func (e *ManagementRolesNotFoundError) Error() string {
	return fmt.Sprintf("node %s not found", e.UUID)
}

func (e *ManagementRolesNotFoundError) Unwrap() error { return ErrNotFound }

// UpdateManagementRolesOff rewrites the switched-off management roles of every
// listed node to next(current) in one transaction: all nodes change or none
// does, and no concurrent batch interleaves. It returns how many nodes changed;
// a node whose set of roles stays the same is not written.
func (s *Store) UpdateManagementRolesOff(uuids []string, next func(current []string) []string) (int, error) {
	unique := map[string]bool{}
	var ordered []string
	for _, uuid := range uuids {
		if !unique[uuid] {
			unique[uuid] = true
			ordered = append(ordered, uuid)
		}
	}
	var changed []string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var nodes []Node
		if err := tx.Select("id", "uuid", "management_roles_off").Where("uuid IN ?", ordered).Find(&nodes).Error; err != nil {
			return err
		}
		byUUID := map[string]Node{}
		for _, n := range nodes {
			byUUID[n.UUID] = n
		}
		for _, uuid := range ordered {
			node, ok := byUUID[uuid]
			if !ok {
				return &ManagementRolesNotFoundError{UUID: uuid}
			}
			current := DecodeStringList(node.ManagementRolesOff)
			updated := next(current)
			if sameStringSet(current, updated) {
				continue
			}
			if err := tx.Model(&Node{}).Where("id = ?", node.ID).Update("management_roles_off", EncodeStringList(updated)).Error; err != nil {
				return err
			}
			changed = append(changed, uuid)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, uuid := range changed {
		s.publishNode(uuid, "management_roles")
	}
	return len(changed), nil
}

func sameStringSet(a, b []string) bool {
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return EncodeStringList(x) == EncodeStringList(y)
}
