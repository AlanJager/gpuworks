//go:build linux

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// clusterStorageSync is the data-only sync the database uses for its log.
func clusterStorageSync(f *os.File) error { return unix.Fdatasync(int(f.Fd())) }
