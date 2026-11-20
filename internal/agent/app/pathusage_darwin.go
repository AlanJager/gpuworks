//go:build darwin

package app

import (
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

func statfsUsage(kind, path string) (proto.PathUsage, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return proto.PathUsage{}, false
	}
	block := uint64(st.Bsize)
	return proto.PathUsage{Kind: kind, Path: path, TotalBytes: storagemount.BlockBytes(uint64(st.Blocks), block), AvailableBytes: storagemount.BlockBytes(uint64(st.Bavail), block)}, true
}
