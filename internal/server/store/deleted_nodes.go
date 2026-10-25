package store

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DeletedNode remembers a deleted node's display name for the statistics and
// metric samples it leaves behind, which name nodes only by UUID; written in
// the transaction that deletes the node.
type DeletedNode struct {
	UUID      string    `gorm:"primaryKey;size:64"`
	Name      string    `gorm:"size:255"`
	DeletedAt time.Time `gorm:"index"`
}

func createDeletedNodes(db *gorm.DB) error {
	return db.AutoMigrate(&DeletedNode{})
}

func rememberDeletedNode(tx *gorm.DB, n Node, at time.Time) error {
	return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&DeletedNode{
		UUID: n.UUID, Name: n.DisplayName(), DeletedAt: at,
	}).Error
}

// DeletedNodes returns the names deleted nodes left, by UUID.
func (s *Store) DeletedNodes(uuids []string) (map[string]DeletedNode, error) {
	out := map[string]DeletedNode{}
	if len(uuids) == 0 {
		return out, nil
	}
	var rows []DeletedNode
	if err := s.reader().Where("uuid IN ?", uuids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.UUID] = r
	}
	return out, nil
}

// NodesDeletedSince lists the UUIDs of the nodes deleted at or after since:
// those whose samples a range starting then may still hold.
func (s *Store) NodesDeletedSince(since time.Time) ([]string, error) {
	var uuids []string
	err := s.reader().Model(&DeletedNode{}).Where("deleted_at >= ?", since.UTC()).Order("uuid").Pluck("uuid", &uuids).Error
	return uuids, err
}
