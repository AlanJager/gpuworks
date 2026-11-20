package engine

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// checkNamedVolumes refuses a launch whose named-volume mounts the driver
// cannot run, before anything is pulled or created. A volume is handed to the
// runtime as `-v <volume>:<target>`, so a name that is not a volume name (a
// host path, extra mount options) or a mount stating a host path too is
// refused on every driver; the server refuses both already, this is the
// agent's own boundary.
func checkNamedVolumes(spec proto.InstanceDesiredState, driver runtime.Driver) error {
	for _, mount := range spec.Mounts {
		if mount.Volume == "" {
			continue
		}
		if mount.Source != "" || !proto.ValidVolumeName(mount.Volume) {
			return withFailure(proto.RuntimeFailure{Code: proto.ErrorMountVolumeInvalid, Resource: mount.Volume},
				fmt.Errorf("mount at %s: %q is not a named volume of its own", mount.Target, mount.Volume))
		}
		if !runtime.SupportsNamedVolumes(driver) {
			return withFailure(proto.RuntimeFailure{Code: proto.ErrorMountVolumeUnsupported, Runtime: driver.Name(), Resource: mount.Volume},
				fmt.Errorf("the %s runtime has no named volumes; mount volume %s at %s with a docker or nerdctl runtime, or mount a host path", driver.Name(), mount.Volume, mount.Target))
		}
	}
	return nil
}
