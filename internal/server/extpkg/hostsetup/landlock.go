package hostsetup

import "gpuworks.ai/gpuworks/pkg/extapi"

// Sandbox is the filesystem access a package's applied host setup adds to
// its plugin's Landlock ruleset.
type Sandbox struct {
	// DeviceDirs get read, write and device ioctl beneath them, and may be
	// listed. A USB device's nodes appear only when it is plugged in, under
	// names nobody knows in advance (/dev/hidrawN, /dev/bus/usb/BBB/DDD), so
	// the grant is the directory, not the node: /dev for hidraw nodes,
	// which also covers /dev/bus/usb for libusb. The boundary between
	// devices is the file mode the rule file sets (group Group, 0660 on the
	// matched nodes only); every other node keeps its own owner and mode.
	DeviceDirs []string
	// ReadPaths are read only: what libusb and hidapi read to find a
	// device (sysfs and udev's device database). The class entries are
	// links into /sys/devices, and Landlock checks the path a link
	// resolves to, so /sys/devices is granted too.
	ReadPaths []string
}

// Landlock is the sandbox steps add, empty when there are none.
func Landlock(steps []extapi.HostSetupStep) Sandbox {
	if len(steps) == 0 {
		return Sandbox{}
	}
	return Sandbox{
		DeviceDirs: []string{"/dev"},
		ReadPaths:  []string{"/sys/bus/usb", "/sys/class/hidraw", "/sys/devices", "/run/udev/data"},
	}
}
