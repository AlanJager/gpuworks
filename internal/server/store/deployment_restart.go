package store

import (
	"time"
)

// Lifecycle events an operator's restart leaves on the deployment timeline.
const (
	// EventDeploymentRestarted: an operator restarted every instance of a
	// deployment, whatever its shape. Params: instances. Rows written before
	// restart covered every shape carry PD_GROUP_RESTARTED; the UI still
	// renders them.
	EventDeploymentRestarted = "DEPLOYMENT_RESTARTED"
	// EventDeploymentRollingRestarted: an operator asked for the deployment's
	// replicas to be replaced one at a time. Params: replicas.
	EventDeploymentRollingRestarted = "DEPLOYMENT_ROLLING_RESTARTED"
	// EventDeploymentRollingStalled: a rolling restart waits on a replica
	// that spent its retry budget; it resumes once that replica is retried.
	// Params: role, replica (1-based), attempts.
	EventDeploymentRollingStalled = "DEPLOYMENT_ROLLING_STALLED"
	// EventReplicaRestarted: an operator restarted one replica through one of
	// its instances. Params: instance (the replica's leader), drain.
	EventReplicaRestarted = "REPLICA_RESTARTED"
)

// HasDeploymentEventSince reports whether the deployment's timeline carries
// an event with this code from since on.
func (s *Store) HasDeploymentEventSince(deploymentID uint, code string, since time.Time) (bool, error) {
	var count int64
	err := s.reader().Model(&InstanceEvent{}).Where("deployment_id = ? AND code = ? AND created_at >= ?", deploymentID, code, since).Count(&count).Error
	return count > 0, err
}

// InstancesWithEvent is which of the named instances carry an event with this
// code on their timeline, in one query.
func (s *Store) InstancesWithEvent(code string, uuids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(uuids) == 0 {
		return out, nil
	}
	var found []string
	if err := s.reader().Model(&InstanceEvent{}).Where("code = ? AND instance_uuid IN ?", code, uuids).Distinct().Pluck("instance_uuid", &found).Error; err != nil {
		return nil, err
	}
	for _, uuid := range found {
		out[uuid] = true
	}
	return out, nil
}

// FinishRollingRestart clears the rolling restart asked for at at. A newer
// request made meanwhile is left in place.
func (s *Store) FinishRollingRestart(id uint, at time.Time) error {
	result := s.db.Model(&Deployment{}).Where("id = ? AND restart_requested_at = ?", id, at).Update("restart_requested_at", nil)
	if result.Error == nil && result.RowsAffected > 0 {
		s.Bus.PublishChange("deployment", uintKey(id), "restart-done")
	}
	return result.Error
}
