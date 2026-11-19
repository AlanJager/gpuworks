//go:build unix

package hostsetup

import (
	"io/fs"
	"syscall"
)

// ownerOf is the uid that owns a file.
func ownerOf(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
