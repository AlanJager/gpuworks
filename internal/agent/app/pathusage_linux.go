//go:build linux

package app

import (
	"syscall"

	"gpuworks.ai/gpuworks/internal/proto"
)

func statfsUsage(kind, path string) (proto.PathUsage, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return proto.PathUsage{}, false
	}
	block := int64(st.Bsize)
	return proto.PathUsage{Kind: kind, Path: path, TotalBytes: int64(st.Blocks) * block, AvailableBytes: int64(st.Bavail) * block}, true
}
