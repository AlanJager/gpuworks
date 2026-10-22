//go:build linux && l4

package nfsfault

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const exportSectors = 768 * 1024 * 1024 / 512

type exportDevice struct{ name, loop, table string }

// All block operations target a newly allocated loop device backed by a
// run-private file, through a uniquely named device-mapper mapping.
func newExportDevice(t *testing.T, root, mount string) exportDevice {
	t.Helper()
	disk := filepath.Join(root, "export.img")
	mustCommand(t, "truncate", "-s", fmt.Sprint(exportSectors*512), disk)
	device := exportDevice{name: fmt.Sprintf("gpuworks-nfs-%d-%d", os.Getpid(), time.Now().UnixNano())}
	device.loop = mustCommand(t, "losetup", "--find", "--show", disk)
	t.Cleanup(func() { mustCommand(t, "losetup", "--detach", device.loop) })
	device.table = fmt.Sprintf("0 %d linear %s 0", exportSectors, device.loop)
	mustCommand(t, "dmsetup", "create", device.name, "--table", device.table)
	t.Cleanup(func() { mustCommand(t, "dmsetup", "remove", device.name) })
	path := "/dev/mapper/" + device.name
	mustCommand(t, "mkfs.ext4", "-q", "-F", path)
	mustCommand(t, "mount", "-o", "nosuid,nodev", path, mount)
	t.Cleanup(func() { mustCommand(t, "umount", mount) })
	t.Logf("isolated export device=%s backing=%s", device.name, disk)
	return device
}

func setDeviceTable(name, table string) error {
	// Do not allow a caller to aim this helper at an existing production map.
	if !strings.HasPrefix(name, "gpuworks-nfs-") {
		return fmt.Errorf("unscoped device name %q", name)
	}
	if out, err := command("dmsetup", "suspend", name); err != nil {
		return fmt.Errorf("suspend: %v %s", err, out)
	}
	out, loadErr := command("dmsetup", "reload", name, "--table", table)
	resumed, resumeErr := command("dmsetup", "resume", name)
	if loadErr != nil || resumeErr != nil {
		return fmt.Errorf("reload=%v %s resume=%v %s", loadErr, out, resumeErr, resumed)
	}
	return nil
}
