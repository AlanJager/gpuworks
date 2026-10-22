//go:build darwin

package storagemount

import "golang.org/x/sys/unix"

func defaultStatfs(path string) (Usage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	bs := int64(st.Bsize)
	return Usage{
		TotalBytes:     int64(st.Blocks) * bs,
		AvailableBytes: int64(st.Bavail) * bs,
		FSType:         darwinFSMagic[unix.ByteSliceToString(st.Fstypename[:])],
	}, nil
}

// darwinFSMagic gives the macOS filesystems that have a Linux counterpart the
// Linux statfs magic number IsNetworkFS knows them by. Any other filesystem,
// local ones included, maps to 0.
var darwinFSMagic = map[string]uint64{
	"nfs":     0x6969,
	"smbfs":   0xFE534D42,
	"macfuse": 0x65735546,
	"osxfuse": 0x65735546,
}
