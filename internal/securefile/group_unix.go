//go:build linux || darwin

package securefile

import (
	"os"
	"syscall"
)

func groupID(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, false
	}
	return int(st.Gid), true
}
