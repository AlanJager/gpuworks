package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// ControlPlaneIdentity names this gpuworks installation on shared storages.
type ControlPlaneIdentity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// controlPlaneMu serializes creating the identity: two first uses at once
// would each make one, and the heartbeat directory would split between them.
var controlPlaneMu sync.Mutex

// ControlPlaneIdentity returns the identity, creating it on first use.
func (s *Store) ControlPlaneIdentity() ControlPlaneIdentity {
	controlPlaneMu.Lock()
	defer controlPlaneMu.Unlock()
	var identity ControlPlaneIdentity
	if raw := s.GetSetting(SettingControlPlane); raw != "" && json.Unmarshal([]byte(raw), &identity) == nil && identity.ID != "" {
		return identity
	}
	identity.ID = auth.NewUUID()
	identity.Name = "gpuworks-" + identity.ID[:8]
	if b, err := json.Marshal(identity); err == nil {
		_ = s.SetSetting(SettingControlPlane, string(b))
	}
	return identity
}

// EnsureControlPlaneIdentity returns an identity only after it is persisted.
func (s *Store) EnsureControlPlaneIdentity() (ControlPlaneIdentity, error) {
	controlPlaneMu.Lock()
	defer controlPlaneMu.Unlock()
	var identity ControlPlaneIdentity
	raw, err := s.LookupSetting(SettingControlPlane)
	if err != nil {
		return identity, fmt.Errorf("read control plane identity: %w", err)
	}
	if raw != "" && json.Unmarshal([]byte(raw), &identity) == nil && identity.ID != "" {
		return identity, nil
	}
	identity.ID = auth.NewUUID()
	identity.Name = "gpuworks-" + identity.ID[:8]
	b, err := json.Marshal(identity)
	if err != nil {
		return identity, err
	}
	if err := s.SetSetting(SettingControlPlane, string(b)); err != nil {
		return identity, fmt.Errorf("save control plane identity: %w", err)
	}
	return identity, nil
}

// SetControlPlaneName renames this installation as other control planes see it.
func (s *Store) SetControlPlaneName(name string) (ControlPlaneIdentity, error) {
	identity := s.ControlPlaneIdentity()
	controlPlaneMu.Lock()
	defer controlPlaneMu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return identity, errors.New("control plane name must be 1-64 characters")
	}
	identity.Name = name
	b, err := json.Marshal(identity)
	if err != nil {
		return identity, err
	}
	return identity, s.SetSetting(SettingControlPlane, string(b))
}
