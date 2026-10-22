package store

import (
	"encoding/json"
	"errors"
)

// StoragePolicy holds the platform-wide storage settings.
type StoragePolicy struct {
	// ReserveFreeBytes is the free space a node cache disk keeps; zero uses
	// the default (the smaller of 10% of the disk and 50 GiB).
	ReserveFreeBytes int64 `json:"reserve_free_bytes"`
}

// DefaultReserveFreeBytes is the reserve when the policy does not set one.
func DefaultReserveFreeBytes(diskTotal int64) int64 {
	const cap = 50 << 30
	tenth := diskTotal / 10
	if tenth > 0 && tenth < cap {
		return tenth
	}
	return cap
}

// StoragePolicy returns the current policy; a missing or corrupt value is the
// zero policy.
func (s *Store) StoragePolicy() StoragePolicy {
	var policy StoragePolicy
	if raw := s.GetSetting(SettingStoragePolicy); raw != "" {
		_ = json.Unmarshal([]byte(raw), &policy)
	}
	if policy.ReserveFreeBytes < 0 {
		policy.ReserveFreeBytes = 0
	}
	return policy
}

// SetStoragePolicy replaces the policy.
func (s *Store) SetStoragePolicy(policy StoragePolicy) error {
	if policy.ReserveFreeBytes < 0 {
		return errors.New("reserve_free_bytes must be non-negative")
	}
	b, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return s.SetSetting(SettingStoragePolicy, string(b))
}
