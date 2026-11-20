//go:build !linux

package cli

import "os"

func clusterStorageSync(f *os.File) error { return f.Sync() }
