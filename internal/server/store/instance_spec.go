package store

import (
	"encoding/json"
	"fmt"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// DesiredState decodes the desired state the instance was launched with and
// orders have since rewritten (the spec column).
func (in *Instance) DesiredState() (proto.InstanceDesiredState, error) {
	var spec proto.InstanceDesiredState
	if in.DesiredStateJSON == "" {
		return spec, fmt.Errorf("instance %s has no desired state", in.UUID)
	}
	if err := json.Unmarshal([]byte(in.DesiredStateJSON), &spec); err != nil {
		return spec, fmt.Errorf("decode instance %s desired state: %w", in.UUID, err)
	}
	return spec, nil
}

// SleepIntent is the sleep level the instance's desired state orders; 0
// (awake) when it has none or cannot be read. It decodes only that field,
// since the gateway asks per request.
func (in *Instance) SleepIntent() int {
	if in == nil || in.DesiredStateJSON == "" {
		return 0
	}
	var spec struct {
		DesiredSleep int `json:"desired_sleep"`
	}
	if json.Unmarshal([]byte(in.DesiredStateJSON), &spec) != nil {
		return 0
	}
	return spec.DesiredSleep
}

// rewriteDesiredState applies change to an instance's desired state inside a
// transaction and stores it when change reports it changed something.
func rewriteDesiredState(tx *gorm.DB, in *Instance, change func(*proto.InstanceDesiredState) bool) error {
	spec, err := in.DesiredState()
	if err != nil {
		return err
	}
	if !change(&spec) {
		return nil
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if err := tx.Model(&Instance{}).Where("id = ?", in.ID).Update("spec", string(raw)).Error; err != nil {
		return err
	}
	in.DesiredStateJSON = string(raw)
	return nil
}
