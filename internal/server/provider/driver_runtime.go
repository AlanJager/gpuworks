package provider

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// DriverFactory optionally constructs a driver with application-owned
// dependencies. The registered prototype must not retain these dependencies:
// multiple servers in one process must have independent driver state.
type DriverFactory interface {
	NewDriver(*store.Store, *Service, provision.Installer) Driver
}

// ConfigureDrivers binds registered drivers to this service before serving
// requests. Existing app-local overrides are preserved. Drivers without a
// factory retain their existing registration behavior.
func (s *Service) ConfigureDrivers(installer provision.Installer) {
	for _, name := range Names() {
		if _, configured := s.drivers[name]; configured {
			continue
		}
		driver, _ := Get(name)
		if factory, ok := driver.(DriverFactory); ok {
			driver = factory.NewDriver(s.store, s, installer)
		}
		s.SetDriver(driver)
	}
}

func (s *Service) configuredDriverNames() []string {
	names := make([]string, 0, len(s.drivers))
	for name := range s.drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
