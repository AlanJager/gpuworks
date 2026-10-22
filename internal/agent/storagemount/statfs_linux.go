//go:build linux

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
		FSType:         uint64(uint32(st.Type)),
	}, nil
}
