package app

import (
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/proto"
)

// statManagedPath reports the filesystem usage of path itself; a path that
// cannot be stat'ed reports zero, which the server treats as unknown.
func statManagedPath(kind, path string) proto.PathUsage {
	if usage, ok := statfsUsage(kind, path); ok {
		return usage
	}
	return proto.PathUsage{Kind: kind, Path: path}
}

// statAgentOwnedPath is statManagedPath for a directory the agent creates on
// first use (the models root before a node's first download, the udocker
// tree): a path that does not exist yet walks up to the nearest existing
// ancestor, the same rule checkModelDownloadSpace applies, so a fresh node
// reports the disk that will hold it instead of 0/0. The reported Path stays
// the requested one. Paths owned by someone else (the docker data root) must
// not walk: a missing /var/lib/docker usually means docker keeps its data
// elsewhere, and the root filesystem's numbers would be wrong, not unknown.
func statAgentOwnedPath(kind, path string) proto.PathUsage {
	probe := path
	for {
		if usage, ok := statfsUsage(kind, probe); ok {
			usage.Path = path
			return usage
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return proto.PathUsage{Kind: kind, Path: path}
		}
		probe = parent
	}
}
