package store

import (
	"encoding/json"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// copyInstanceGPUIdentsIntoSpec gives the desired state of instances placed
// before cards were bound by identity the binding migration 143 recorded in
// their gpu_idents column. That migration filled the column only, and the
// agent is replayed the desired state, not the column: an instance launched
// before it reached the agent with no identities, so the agent believed none
// of its cards were its own and reported its own engine as a process outside
// the platform, taking those cards off the scheduler.
//
// A spec that already names its cards, a column that does not cover every
// device index, and a spec that cannot be read are left alone. Re-running
// changes nothing.
func copyInstanceGPUIdentsIntoSpec(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Instance{}) || !db.Migrator().HasColumn(&Instance{}, "GPUIdents") {
		return nil
	}
	var instances []Instance
	if err := db.Select("ID", "UUID", "GPUIdents", "DesiredStateJSON").
		Where("gpu_idents <> '' AND spec <> ''").Order("id asc").Find(&instances).Error; err != nil {
		return err
	}
	for i := range instances {
		inst := &instances[i]
		var idents []string
		if json.Unmarshal([]byte(inst.GPUIdents), &idents) != nil || len(idents) == 0 {
			continue
		}
		if _, err := inst.DesiredState(); err != nil {
			continue
		}
		err := rewriteDesiredState(db, inst, func(spec *proto.InstanceDesiredState) bool {
			if len(spec.GPUIdents) > 0 || len(spec.GPUIndexes) != len(idents) {
				return false
			}
			spec.GPUIdents = idents
			return true
		})
		if err != nil {
			return err
		}
	}
	return nil
}
