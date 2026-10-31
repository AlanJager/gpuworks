package store

import "gorm.io/gorm"

// CreateInstances persists every rank of one replica generation in a single
// transaction, so a replica is written whole or not at all: a rank that fails
// to insert takes the ranks before it with it. reads[i], when given, are the
// model files insts[i] mounts. Each instance's created event is published only
// after the commit.
func (s *Store) CreateInstances(insts []*Instance, reads [][]InstanceModelFile) error {
	var created []InstanceEvent
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		created = created[:0]
		for i, inst := range insts {
			ev, err := s.createInstance(tx, inst)
			if err != nil {
				return err
			}
			created = append(created, ev)
			if i >= len(reads) {
				continue
			}
			for _, read := range reads[i] {
				read.ID, read.InstanceID = 0, inst.ID
				if err := tx.Create(&read).Error; err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, ev := range created {
		s.observe(ev, nil)
	}
	for _, inst := range insts {
		s.Bus.PublishChange("instance", inst.UUID, "created")
	}
	return nil
}
