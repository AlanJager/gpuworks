package store

import (
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// replicaRemovedMessage is what an instance drained by a scale-down says.
const replicaRemovedMessage = "replica removed"

// ScaleDeploymentReplicas changes an ordinary deployment's replica count
// from the count the caller read to replicas and marks the instances the
// operator named as draining, in one transaction, so the controller never
// sees the new count with the named instances still filling it. Both parts
// are conditional: ErrStateChanged when the count or a named instance moved
// on since the caller read them (nothing is written). The events are
// published after the commit.
func (s *Store) ScaleDeploymentReplicas(id uint, from, replicas int, drain []string) error {
	var drained []Instance
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&Deployment{}).Where("id = ? AND replicas = ?", id, from).Update("replicas", replicas)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var count int64
			if err := tx.Model(&Deployment{}).Where("id = ?", id).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return ErrNotFound
			}
			return ErrStateChanged
		}
		var err error
		drained, err = transitionInstances(tx, id, drain, ReplicaFillingStates(), proto.InstDraining, replicaRemovedMessage)
		return err
	})
	if err != nil {
		return err
	}
	s.announceTransition(drained, proto.InstDraining, replicaRemovedMessage)
	s.Bus.PublishChange("deployment", uintKey(id), "replicas")
	return nil
}

// ReplicaUnitsPatch is what listing an ordinary multi-node deployment's
// replicas writes (planning.PlanReplicaUnits derives it from the list): the
// placement that lists them, the nodes they run on (the first replica's
// leader first), that leader, and the replica count.
type ReplicaUnitsPatch struct {
	Placement string
	NodeIDs   string
	NodeID    uint
	Replicas  int
}

// SetDeploymentReplicaUnits writes a multi-node deployment's listed replicas
// in one update. A stored master address belongs to the first replica's
// leader it was resolved for; when the list leads the first replica from
// another node it is cleared, so every replica rendezvous on its own leader.
// A replica left out of the list drains by the controller's hand.
func (s *Store) SetDeploymentReplicaUnits(id uint, patch ReplicaUnitsPatch) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var dep Deployment
		if err := tx.Select("id", "node_id").First(&dep, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		updates := map[string]any{"placement": patch.Placement, "node_ids": patch.NodeIDs, "node_id": patch.NodeID, "replicas": patch.Replicas}
		if dep.NodeID != patch.NodeID {
			updates["master_addr"] = ""
		}
		return tx.Model(&Deployment{}).Where("id = ?", id).Updates(updates).Error
	})
	if err == nil {
		s.Bus.PublishChange("deployment", uintKey(id), "replicas")
	}
	return err
}
