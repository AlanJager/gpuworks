package selfinstall

import "golang.org/x/sys/unix"

func uninstallMountPoints() ([]string, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	stats := make([]unix.Statfs_t, n+16)
	n, err = unix.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	var points []string
	for _, stat := range stats[:n] {
		points = append(points, unix.ByteSliceToString(stat.Mntonname[:]))
	}
	return points, nil
}
