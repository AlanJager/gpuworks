//go:build unix

package launchd

import (
	"io/fs"
	"syscall"
)

func fileUID(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
