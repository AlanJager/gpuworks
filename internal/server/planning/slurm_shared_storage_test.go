package planning

import (
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSlurmSharedStorageIsSuggestedOnlyForACloudEntryWithASharedDirJoinedNodesAndNoSharedStorage(t *testing.T) {
	account := uint(3)
	entry := store.SlurmCluster{UUID: "9171f8d3", Name: "slurm25.05.5-cluster-20261118", ProviderID: &account}
	got, ok := SlurmSharedStorageFor(entry, "/opt/gpuworks-models", 1, false)
	want := SlurmSharedStorage{Name: "slurm25-05-5-cluster-20261118-models", Path: "/opt/gpuworks-models", LabelKey: "gpuworks.ai/slurm-cluster", LabelValue: "9171f8d3"}
	if !ok || got != want {
		t.Fatalf("suggestion = %+v %v; want %+v", got, ok, want)
	}
	revoked := entry
	now := time.Now()
	revoked.RevokedAt = &now
	own := entry
	own.ProviderID = nil
	for name, c := range map[string]struct {
		entry     store.SlurmCluster
		sharedDir string
		joined    int
		hasShared bool
	}{
		"no node joined":                   {entry, "/opt/gpuworks-models", 0, false},
		"a shared storage":                 {entry, "/opt/gpuworks-models", 2, true},
		"revoked":                          {revoked, "/opt/gpuworks-models", 2, false},
		"connected on its own":             {own, "/opt/gpuworks-models", 2, false},
		"a cloud that knows no shared dir": {entry, "", 2, false},
	} {
		if s, ok := SlurmSharedStorageFor(c.entry, c.sharedDir, c.joined, c.hasShared); ok {
			t.Errorf("%s: suggested %+v", name, s)
		}
	}
}

func TestSlurmStorageName(t *testing.T) {
	for in, want := range map[string]string{
		"lab":          "lab-models",
		"GPU Lab / 北京": "gpu-lab-models",
		"--":           "slurm-models",
		"":             "slurm-models",
	} {
		if got := slurmStorageName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := slurmStorageName(strings.Repeat("a", 80)); len(got) > 63 || !strings.HasSuffix(got, "-models") {
		t.Errorf("long name = %q (%d)", got, len(got))
	}
}
