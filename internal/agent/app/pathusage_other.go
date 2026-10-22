//go:build !linux && !darwin

package app

import "gpuworks.ai/gpuworks/internal/proto"

// statfsUsage has no implementation on this platform; statManagedPath reports
// the zero usage for every path.
func statfsUsage(kind, path string) (proto.PathUsage, bool) {
	return proto.PathUsage{}, false
}
