package store

// seedDefaults initializes shared configuration without process-specific recovery.
// Replicated open uses this instead of the single-member startup seed. Image
// and storage defaults stay on the current server startup path.
func (s *Store) seedDefaults() error {
	if err := s.EnsureTerminalSettings(); err != nil {
		return err
	}
	return s.EnsureDeploymentPolicy()
}
