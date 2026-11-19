package nerdctl

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// TestBuildRunArgsMountsNamedVolumes proves a named-volume mount reaches
// `nerdctl run` as the volume name, not as an empty host path (`-v :/data`).
func TestBuildRunArgsMountsNamedVolumes(t *testing.T) {
	args := New().buildRunArgs(runtime.WorkloadSpec{Name: "gpuworks-test", Image: "fake", Mounts: []runtime.Mount{
		{Volume: "myvol", Target: "/data"},
		{Volume: "hf-cache", Target: "/root/.cache", ReadOnly: true},
		{Source: "/srv/models/qwen", Target: "/model", ReadOnly: true},
	}})
	var got []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-v" {
			got = append(got, args[i+1])
		}
	}
	want := []string{"myvol:/data", "hf-cache:/root/.cache:ro", "/srv/models/qwen:/model:ro"}
	if !slices.Equal(got, want) {
		t.Fatalf("nerdctl -v values = %q, want %q (args %q)", got, want, args)
	}
	if !runtime.SupportsNamedVolumes(New()) {
		t.Fatal("nerdctl driver does not claim named volumes")
	}
}
