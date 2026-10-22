package store

import (
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// DeploymentChangeDGPUQuota is the change reason a dGPU quota edit publishes
// on the deployment topic: the controller pushes the instances whose limit
// changed to their nodes.
const DeploymentChangeDGPUQuota = "dgpu_quota"

// EventDeploymentDGPUQuotaChanged: an operator changed a deployment's dGPU
// memory limit. Params: from_bytes, to_bytes, live (instances held to it in
// place), pending (instances that take it at their next restart).
const EventDeploymentDGPUQuotaChanged = "DEPLOYMENT_DGPU_QUOTA_CHANGED"

// SetDeploymentDGPUQuota sets a deployment's dGPU memory limit and, on the
// named live instances, the limit their desired state carries, in one
// transaction. The deployment's limit is what its next instances start
// under; an instance's own is what it runs under and what the GPU ledger
// charges, so an instance left out keeps its limit until it is replaced.
func (s *Store) SetDeploymentDGPUQuota(id uint, vramLimitBytes int64, live []string) error {
	var rewritten []string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&Deployment{}).Where("id = ?", id).Update("VRAMLimitBytes", vramLimitBytes)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		if len(live) == 0 {
			return nil
		}
		var insts []Instance
		if err := tx.Where("deployment_id = ? AND uuid IN ?", id, live).Find(&insts).Error; err != nil {
			return err
		}
		for i := range insts {
			changed := false
			if err := rewriteDesiredState(tx, &insts[i], func(spec *proto.InstanceDesiredState) bool {
				if spec.VRAMLimitBytes == vramLimitBytes {
					return false
				}
				spec.VRAMLimitBytes = vramLimitBytes
				changed = true
				return true
			}); err != nil {
				return err
			}
			if changed {
				rewritten = append(rewritten, insts[i].UUID)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.Bus.PublishChange("deployment", uintKey(id), DeploymentChangeDGPUQuota)
	for _, uuid := range rewritten {
		s.Bus.PublishChange("instance", uuid, "desired-state")
	}
	return nil
}
