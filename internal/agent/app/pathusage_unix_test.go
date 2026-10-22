//go:build linux || darwin

package app

import (
	"path/filepath"
	"testing"
)

// TestStatAgentOwnedPathWalksToNearestAncestor pins the fresh-node path: the
// models root legitimately does not exist until the first download, and the
// usage snapshot must report the filesystem that will hold it (the nearest
// existing ancestor) instead of 0/0 — without creating anything. Creating the
// directory eagerly was rejected in review: a typo'd storage-page path, or a
// not-yet-mounted target, would silently materialize on the root disk and
// fill the system volume with weights.
func TestStatAgentOwnedPathWalksToNearestAncestor(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "models", "nested")

	usage := statAgentOwnedPath("models", missing)

	if usage.Kind != "models" || usage.Path != missing {
		t.Fatalf("reported %q/%q, want models/%q", usage.Kind, usage.Path, missing)
	}
	if usage.TotalBytes <= 0 || usage.AvailableBytes <= 0 {
		t.Fatalf("missing dir reported no capacity: %+v", usage)
	}
	if _, ok := statfsUsage("models", missing); ok {
		t.Fatal("precondition failed: the target exists, the walk was not exercised")
	}
}

// TestStatManagedPathDoesNotWalk pins the other half: a path the agent does
// not own (the docker data root) stays unknown when missing, because the
// nearest ancestor's filesystem is not where that data lives.
func TestStatManagedPathDoesNotWalk(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "docker")

	usage := statManagedPath("docker", missing)

	if usage.Kind != "docker" || usage.Path != missing || usage.TotalBytes != 0 || usage.AvailableBytes != 0 {
		t.Fatalf("missing docker root = %+v, want zero usage", usage)
	}
	existing := statManagedPath("docker", t.TempDir())
	if existing.TotalBytes <= 0 || existing.AvailableBytes <= 0 {
		t.Fatalf("existing path reported no capacity: %+v", existing)
	}
}
