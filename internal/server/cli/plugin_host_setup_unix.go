//go:build unix

package cli

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// dataOwner is the account that owns dataDir, the service account of an
// installation; "" when it is root or cannot be read.
func dataOwner(dataDir string) string {
	info, err := os.Stat(dataDir)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return ""
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
	if err != nil {
		return ""
	}
	return u.Username
}
