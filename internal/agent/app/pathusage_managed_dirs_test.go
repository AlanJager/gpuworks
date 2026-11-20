package app

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The managed directories' disk is reported like the models root - a root
// not created yet reports the disk that will hold it - and names the measured
// paths on the same filesystem, so the server charges a launch's image and
// weights there only when they share it.
func TestManagedDirUsageNamesThePathsOnItsFilesystem(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "runtime-mounts", proto.MountPurposeManagedDir)
	prev := doctorFilesystemOf
	t.Cleanup(func() { doctorFilesystemOf = prev })
	doctorFilesystemOf = func(path string) (uint64, bool) {
		switch {
		case strings.HasPrefix(path, base):
			return 1, true
		case path == "/elsewhere":
			return 2, true
		}
		return 0, false
	}
	others := []proto.PathUsage{
		{Kind: "models", Path: filepath.Join(base, "models"), TotalBytes: 10, AvailableBytes: 5},
		{Kind: "docker", Path: "/elsewhere", TotalBytes: 10, AvailableBytes: 5},
		{Kind: "udocker", Path: filepath.Join(base, "udocker")}, // not measured: never shares
		{Kind: "unknown", Path: "/nowhere", TotalBytes: 10, AvailableBytes: 5},
	}
	usage := managedDirUsage(root, others)
	if usage.Kind != proto.PathKindManagedDirs || usage.Path != root {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.TotalBytes <= 0 {
		t.Skip("statfs reports no size for the temporary directory on this platform")
	}
	if !slices.Equal(usage.SharesDiskWith, []string{"models"}) {
		t.Fatalf("shares = %v, want only the models root", usage.SharesDiskWith)
	}

	// A filesystem the agent cannot identify shares nothing it could prove.
	doctorFilesystemOf = func(string) (uint64, bool) { return 0, false }
	if usage := managedDirUsage(root, others); usage.SharesDiskWith != nil {
		t.Fatalf("shares = %v on an unidentified filesystem", usage.SharesDiskWith)
	}
}
