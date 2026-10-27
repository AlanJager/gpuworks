package store

import "gorm.io/gorm"

// addDeploymentNodeReplaceAfter adds how long a node may stay offline before
// a deployment's replicas on it move. Existing deployments take the default
// (600 s): replacement is on unless a deployment turns it off.
func addDeploymentNodeReplaceAfter(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) || db.Migrator().HasColumn(&Deployment{}, "NodeReplaceAfterSec") {
		return nil
	}
	return db.Migrator().AddColumn(&Deployment{}, "NodeReplaceAfterSec")
}

// SetDeploymentNodeReplaceAfter sets how long a node may stay offline before
// the deployment's replicas on it move elsewhere; 0 never moves them.
func (s *Store) SetDeploymentNodeReplaceAfter(id uint, afterSec int) error {
	res := s.db.Model(&Deployment{}).Where("id = ?", id).Update("node_replace_after_sec", afterSec)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.Bus.PublishChange("deployment", uintKey(id), "node-replace")
	return nil
}

// EventKindAutomation is the timeline kind of what the platform did on its
// own for a deployment, such as moving a replica off a lost node.
const EventKindAutomation = "automation"
